"""Conformance of the records this runtime actually persists, against the frozen schemas.

Every instance here is read back out of the store after the runtime wrote it, not hand-built, so
what is validated is what a real deployment would produce.

Two things this suite is careful about. A skip is not a pass: under RELAY_CONFORMANCE_REQUIRED the
gate FAILS instead of skipping, so an evidence run cannot exit successfully having validated
nothing. And a permissive validator is not a pass either: every positive has a negative that must
be rejected, so a misconfigured run shows up as a failure rather than a clean sheet.

Recorded limitation: the available jsonschema has no working date-time format checker, so a
malformed timestamp is NOT caught here. Format validation is unverified; the semantic checks that
matter live in the rest of the suite.
"""

import copy
import json
import os
import unittest
from pathlib import Path

import codex_session_relay
from codex_session_relay import identity, manifest
from codex_session_relay.ack import AckService
from codex_session_relay.daemon import RelayDaemon
from codex_session_relay.delivery import COMPLETION, REVISION, DeliveryService
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.models import TurnRef
from codex_session_relay.receipts import ReceiptIntake, contract_record
from codex_session_relay.reconcile import Reconciler
from codex_session_relay.registry import Registry, contract_record as relationship_record

from .support import CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, RelayTestCase

SCHEMA_DIR = Path(codex_session_relay.__file__).parent / "schema"
SCHEMAS = {
    "relationship": "relationship.json",
    "completion-receipt": "completion-receipt.json",
    "delivery-attempt": "delivery-attempt.json",
    "acknowledgement": "acknowledgement.json",
    "verification-verdict": "verification-verdict.json",
}
REQUIRED = os.environ.get("RELAY_CONFORMANCE_REQUIRED") == "1"

try:
    from jsonschema import Draft7Validator
except ImportError:
    Draft7Validator = None

COUNTS = {name: 0 for name in SCHEMAS}
SKIPS = []


def _load(name):
    return json.loads((SCHEMA_DIR / SCHEMAS[name]).read_text())


def _validator(name):
    return Draft7Validator(_load(name))


class ConformanceCase(RelayTestCase):
    def setUp(self):
        if Draft7Validator is None:
            if REQUIRED:
                self.fail("jsonschema is required for the conformance evidence run")
            SKIPS.append(self.id())
            self.skipTest("jsonschema is unavailable in this interpreter")
        super().setUp()
        self.adapter = FakeHostAdapter(self.clock)
        self.adapter.add_thread(PARENT)
        self.adapter.add_thread(CHILD)
        self.intake = ReceiptIntake(self.store, self.registry, self.clock)
        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.ack = AckService(
            self.store, self.registry, self.intake, self.delivery, self.clock
        )
        self.reconciler = Reconciler(self.store, self.registry, self.delivery, self.clock)

    def valid(self, name, instance, label):
        errors = sorted(_validator(name).iter_errors(instance), key=lambda e: e.path)
        self.assertEqual(
            [f"{list(e.path)}: {e.message}" for e in errors], [],
            f"{label} should satisfy {name}",
        )
        COUNTS[name] += 1

    def invalid(self, name, instance, label):
        self.assertTrue(
            list(_validator(name).iter_errors(instance)),
            f"{label} must be rejected by {name}; a validator that accepts it proves nothing",
        )


class Relationships(ConformanceCase):
    def test_both_anchor_states_validate(self):
        pending = self.register(dispatch_turn_id=None)
        record = relationship_record(pending)
        self.valid("relationship", record, "anchor_pending relationship")
        self.assertEqual(record["generations"][0]["anchorState"], "anchor_pending")
        self.assertIsNone(record["generations"][0]["dispatchTurnId"])

        self.registry.bind_anchor(
            pending["relationshipId"], 1, dispatch_turn_id=DISPATCH_TURN,
            source="dispatch_receipt",
        )
        bound = relationship_record(self.registry.get(pending["relationshipId"]))
        self.valid("relationship", bound, "bound relationship")
        self.assertEqual(bound["generations"][0]["anchorState"], "bound")
        self.assertTrue(bound["generations"][0]["boundAt"])

    def test_negatives_are_rejected(self):
        record = relationship_record(self.register())
        broken = copy.deepcopy(record)
        broken["generations"][0]["dispatchTurnId"] = None
        self.invalid("relationship", broken, "bound anchor with no turn id")
        broken = copy.deepcopy(record)
        broken["relationshipId"] = "not-a-relationship-id"
        self.invalid("relationship", broken, "malformed relationship id")
        broken = copy.deepcopy(record)
        broken["surprise"] = 1
        self.invalid("relationship", broken, "unknown property")
        broken = copy.deepcopy(record)
        broken["authorizedScope"]["artifactRoots"] = []
        self.invalid("relationship", broken, "empty artifact roots")


class Receipts(ConformanceCase):
    def _ready(self, relationship, *, status="completed", turn=DISPATCH_TURN):
        path = self.artifact("out.txt", "deliverable " + status + turn)
        payload = self.ready_payload(
            relationship, [path], turn=self.assigned_turn(status, turn=turn)
        )
        return self.accept(payload), payload["eventId"]

    def test_every_producer_and_outcome_branch_validates(self):
        relationship = self.register()
        stored, _event = self._ready(relationship)
        self.valid("completion-receipt", contract_record(stored), "child ready_for_review")

        staged = self.register(issue_key="REL-STAGED", dispatch_request_id="d-staged")
        stored_staged, _ = self._ready(staged, status="inProgress")
        self.valid("completion-receipt", contract_record(stored_staged), "staged inProgress")

        for outcome, status in (("failed", "failed"), ("interrupted", "interrupted"),
                                ("blocked_needs_input", "completed")):
            other = self.register(
                issue_key=f"REL-{outcome}", dispatch_request_id=f"d-{outcome}"
            )
            payload = self.execution_payload(
                other, outcome, turn=self.assigned_turn(status)
            )
            self.valid(
                "completion-receipt", contract_record(self.accept(payload)),
                f"child {outcome}",
            )

        for status in ("failed", "interrupted"):
            observed = self.register(
                issue_key=f"REL-daemon-{status}", dispatch_request_id=f"d-daemon-{status}"
            )
            receipt = self.intake.daemon_observation(
                observed["relationshipId"], self.assigned_turn(status)
            )
            record = contract_record(receipt)
            self.valid("completion-receipt", record, f"daemon {status}")
            self.assertIsNone(record["attempt"])
            self.assertIsNone(record["manifest"])
            self.assertEqual(record["revisionHash"], "0" * 64)
            self.assertEqual(record["turnRef"]["turnStatus"], status)

    def test_negatives_are_rejected(self):
        relationship = self.register()
        stored, _event = self._ready(relationship)
        record = contract_record(stored)
        for label, mutate in (
            ("ready with a null manifest", lambda r: r.update(manifest=None)),
            ("ready with the sentinel digest", lambda r: r.update(revisionHash="0" * 64)),
            ("zero attempt", lambda r: r.update(attempt=0)),
            ("boolean generation", lambda r: r.update(executionGeneration=True)),
            ("malformed event id", lambda r: r.update(eventId="nope")),
            ("unknown outcome", lambda r: r.update(outcome="finished")),
            ("unknown property", lambda r: r.update(surprise=1)),
            ("relative manifest path",
             lambda r: r["manifest"][0].update(path="relative/path")),
            ("unknown turn status", lambda r: r["turnRef"].update(turnStatus="gone")),
        ):
            broken = copy.deepcopy(record)
            mutate(broken)
            self.invalid("completion-receipt", broken, label)

        daemon = contract_record(self.intake.daemon_observation(
            self.register(issue_key="REL-neg", dispatch_request_id="d-neg")["relationshipId"],
            self.assigned_turn("failed"),
        ))
        broken = copy.deepcopy(daemon)
        broken["attempt"] = 1
        self.invalid("completion-receipt", broken, "daemon observation with an attempt")
        broken = copy.deepcopy(daemon)
        broken["turnRef"]["turnStatus"] = "interrupted"
        self.invalid("completion-receipt", broken, "daemon failed on an interrupted turn")


class Attempts(ConformanceCase):
    def _attempt(self, outcome, *, issue, approval="never"):
        relationship = self.register(issue_key=issue, dispatch_request_id=f"d-{issue}")
        path = self.artifact(f"{issue}.txt", "payload " + issue)
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        self.adapter.threads[PARENT].approval_policy = approval
        if outcome:
            self.adapter.script(outcome)
        # The anti-flood interval applies per recipient, so successive cases move the clock
        # exactly as a daemon tick would rather than being rate-limited into returning None.
        self.clock.advance(3600)
        record = self.delivery.attempt(payload["eventId"], self.adapter, now=self.clock.now())
        self.assertIsNotNone(record, f"{issue} produced no attempt")
        return record

    def test_every_delivery_state_branch_validates(self):
        cases = [
            (None, "dispatched", "REL-dispatch"),
            ("busy", "deferred_busy", "REL-busy"),
            ("read_fail", "withheld_pre_send", "REL-read"),
            ("resume_fail", "withheld_pre_send", "REL-resume"),
            ("turn_start_fail", "held_uncertain", "REL-turnstart"),
            ("initialize_fail", "held_uncertain", "REL-init"),
            ("transport_unknown", "held_uncertain", "REL-transport"),
            ("in_progress", "held_uncertain", "REL-unfinished"),
        ]
        for outcome, expected, issue in cases:
            record = self._attempt(outcome, issue=issue)
            self.assertEqual(record["deliveryState"], expected, issue)
            self.valid("delivery-attempt", _attempt_record(record), f"{expected} attempt")

        inbox = self._attempt("approval_policy", issue="REL-inbox", approval="on-request")
        self.assertEqual(inbox["deliveryState"], "inbox_only")
        self.valid("delivery-attempt", _attempt_record(inbox), "inbox_only attempt")
        self.assertEqual(inbox["recipientApprovalPolicy"], "on-request")

    def test_a_reconciled_attempt_keeps_a_valid_record(self):
        record = self._attempt("in_progress", issue="REL-recon")
        self.adapter.start_turn(
            PARENT, status="completed", text=f"...{record['requestId']}..."
        )
        self.reconciler.reconcile_attempt(record["requestId"], self.adapter)
        row = self.store.one(
            "SELECT record FROM attempts WHERE request_id = ?", (record["requestId"],)
        )
        stored = json.loads(row["record"])
        self.valid("delivery-attempt", stored, "reconciled held_uncertain attempt")
        self.assertEqual(stored["reconciliation"]["affirmativeEvidence"], "turn_found")

    def test_negatives_are_rejected(self):
        record = _attempt_record(self._attempt(None, issue="REL-neg-attempt"))
        for label, mutate in (
            ("dispatched with no turn", lambda r: r.update(turnId=None)),
            ("uncertain but retry safe",
             lambda r: r.update(deliveryState="held_uncertain", retrySafe=True)),
            ("retry safe after turn/start",
             lambda r: r.update(retrySafe=True, sendAttempted="no",
                                transportReceiptStatus="failed",
                                deliveryState="withheld_pre_send",
                                failedOperation="turn/start")),
            ("unfinished but certain",
             lambda r: r.update(transportReceiptStatus="in_progress_or_unknown",
                                sendAttempted="yes")),
            ("malformed request id", lambda r: r.update(requestId="del-x-a1")),
            ("unknown property", lambda r: r.update(surprise=1)),
        ):
            broken = copy.deepcopy(record)
            mutate(broken)
            self.invalid("delivery-attempt", broken, label)


class Acknowledgements(ConformanceCase):
    def _dispatched(self, issue):
        relationship = self.register(issue_key=issue, dispatch_request_id=f"d-{issue}")
        path = self.artifact(f"{issue}.txt", "payload " + issue)
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        self.delivery.attempt(payload["eventId"], self.adapter, now=self.clock.now())
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        return relationship, payload["eventId"], turn

    def test_accepted_and_rejected_branches_validate(self):
        _relationship, event_id, turn = self._dispatched("REL-ack-yes")
        accepted = self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        record = {k: v for k, v in accepted.items() if not k.startswith("_")}
        self.valid("acknowledgement", record, "accepted acknowledgement")
        self.assertIsNone(record["rejectionReason"])

        relationship, other_event, other_turn = self._dispatched("REL-ack-no")
        self.registry.open_generation(
            relationship["relationshipId"], dispatch_request_id="later",
            reason="needs_changes_revision", dispatch_turn_id="later-turn",
        )
        rejected = self.ack.acknowledge(
            other_event, ack_turn_id=other_turn.turn_id,
            ack_proof=identity.ack_proof(other_event, other_turn.turn_id), accepted=False,
            rejection_reason="stale_generation", adapter=self.adapter,
        )
        record = {k: v for k, v in rejected.items() if not k.startswith("_")}
        self.valid("acknowledgement", record, "rejected acknowledgement")
        self.assertEqual(record["rejectionReason"], "stale_generation")

    def test_negatives_are_rejected(self):
        _relationship, event_id, turn = self._dispatched("REL-ack-neg")
        accepted = self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        record = {k: v for k, v in accepted.items() if not k.startswith("_")}
        for label, mutate in (
            ("accepted with a reason", lambda r: r.update(rejectionReason="duplicate_event")),
            ("rejected with no reason", lambda r: r.update(accepted=False)),
            ("malformed proof", lambda r: r.update(ackProof="short")),
            ("empty ack turn", lambda r: r.update(ackTurnId="")),
            ("unknown property", lambda r: r.update(surprise=1)),
        ):
            broken = copy.deepcopy(record)
            mutate(broken)
            self.invalid("acknowledgement", broken, label)


class Verdicts(ConformanceCase):
    def _acknowledged(self, issue, *, recipients=None):
        relationship = self.register(
            issue_key=issue, dispatch_request_id=f"d-{issue}", recipients=recipients
        )
        path = self.artifact(f"{issue}.txt", "payload " + issue)
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        self.delivery.attempt(payload["eventId"], self.adapter, now=self.clock.now())
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.ack.acknowledge(
            payload["eventId"], ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(payload["eventId"], turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        return relationship, payload["eventId"]

    def test_all_four_verdicts_validate(self):
        for verdict in ("verified", "unverified", "aborted"):
            _relationship, event_id = self._acknowledged(f"REL-v-{verdict}")
            record = self.ack.record_verdict(
                event_id, verdict=verdict, verdict_turn_id=f"verdict-{verdict}"
            )
            self.valid("verification-verdict", record, f"{verdict} verdict")

        relationship, event_id = self._acknowledged(
            "REL-v-needs", recipients=[PARENT, CHILD]
        )
        record = self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="verdict-needs"
        )
        self.valid("verification-verdict", record, "needs_changes verdict")
        # The schema does not enforce these, so they are asserted separately.
        self.assertEqual(record["nextExecutionGeneration"], 2)
        revision = self.store.one(
            "SELECT * FROM deliveries WHERE kind = ? AND relationship_id = ?",
            (REVISION, relationship["relationshipId"]),
        )
        self.assertEqual(revision["recipient_task_id"], CHILD)

    def test_negatives_are_rejected(self):
        _relationship, event_id = self._acknowledged("REL-v-neg")
        record = self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="verdict-neg"
        )
        for label, mutate in (
            ("unknown verdict", lambda r: r.update(verdict="maybe")),
            ("missing turn", lambda r: r.pop("verdictTurnId")),
            ("malformed event id", lambda r: r.update(eventId="nope")),
            ("unknown property", lambda r: r.update(surprise=1)),
        ):
            broken = copy.deepcopy(record)
            mutate(broken)
            self.invalid("verification-verdict", broken, label)


class ReverseDirectionExclusion(ConformanceCase):
    """The revision record is outside the parent-facing schemas, and that is asserted."""

    def test_the_excluded_records_are_counted_and_described(self):
        relationship = self.register(recipients=[PARENT, CHILD])
        path = self.artifact("out.txt", "payload")
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        self.delivery.enqueue(payload["eventId"])
        self.delivery.attempt(payload["eventId"], self.adapter, now=self.clock.now())
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, status="inProgress")
        self.ack.acknowledge(
            payload["eventId"], ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(payload["eventId"], turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(
            payload["eventId"], verdict="needs_changes", verdict_turn_id="verdict-1"
        )

        excluded, included, unknown = [], [], []
        for row in self.store.all("SELECT * FROM deliveries"):
            if row["kind"] == REVISION:
                excluded.append(row)
            elif row["kind"] == COMPLETION:
                included.append(row)
            else:
                unknown.append(row["kind"])
        self.assertEqual(unknown, [], "an unrecognised delivery kind fails rather than passing")
        self.assertGreater(len(excluded), 0, "the exclusion must be exercised, not assumed")
        self.assertGreater(len(included), 0)

        for row in excluded:
            event = self.intake.row(row["event_id"])
            self.assertEqual(event["producer"], "relay")
            self.assertEqual(event["outcome"], "revision_request")
            self.assertEqual(row["recipient_task_id"], CHILD)
            self.assertEqual(event["execution_generation"], 2)
            stored = json.loads(event["receipt"])
            self.assertEqual(stored["supersedesEvent"], payload["eventId"])
            self.assertIn("contract v1 defines no record", stored["note"])


class ZzConformanceGate(unittest.TestCase):
    """A skip is not a pass, and neither is a validator that accepts everything.

    Named to sort last: unittest runs classes in alphabetical order within a module, and this
    gate asserts on counts the other classes populate.
    """

    def test_conformance_actually_ran(self):
        if not REQUIRED:
            self.skipTest("set RELAY_CONFORMANCE_REQUIRED=1 for the evidence run")
        self.assertIsNotNone(Draft7Validator, "the validator must be available")
        self.assertEqual(len(SCHEMAS), 5)
        for name in SCHEMAS:
            schema = _load(name)
            Draft7Validator.check_schema(schema)
            self.assertGreater(COUNTS[name], 0, f"no instance was validated against {name}")
        self.assertEqual(SKIPS, [], "no conformance case may be skipped in the evidence run")

    def test_the_packaged_schemas_match_the_contract(self):
        import hashlib

        expected = {
            "acknowledgement.json":
                "193c2a1dbdb6197f852aaa38c6b8b4e55ba804ffc66e7b2a73925365b133eb7c",
            "completion-receipt.json":
                "8111438e60b46b209a33902dd9080426953dfaaf2a7025cb47c2336d72b49317",
            "delivery-attempt.json":
                "e821647e35bee9179321332d8b7df06f0fc61f2da49c7b74fb6128b8802650d1",
            "relationship.json":
                "c8ebaf4559fa1ac6df26d98c4214938caf78bd8c61659bce026da6a3595a4b90",
            "verification-verdict.json":
                "0b3f8f4b061cff2992fc60a7c1f45dec6f803116894735751c40df3a8d356af9",
        }
        for name, digest in expected.items():
            data = (SCHEMA_DIR / name).read_bytes()
            self.assertEqual(hashlib.sha256(data).hexdigest(), digest, name)


def _attempt_record(record):
    return {k: v for k, v in record.items() if not k.startswith("_")}


class CriteriaItemsStayOpen(unittest.TestCase):
    """The restoration declaration rides on a finding, which works only while items are open.

    verification-verdict.json freezes additionalProperties on the verdict RECORD and leaves
    its criteria items alone, and that asymmetry is the whole reason a correction can say
    which finding carries its restoration block without a schema change or a store migration.
    Closing the items would invalidate every stored verdict that carries a declaration. The
    packaged digests already catch the edit; this says what the edit would cost, so whoever
    makes it reads a consequence rather than a hash mismatch.
    """

    def test_the_record_is_closed_and_its_criteria_items_are_not(self):
        schema = _load("verification-verdict")
        self.assertIs(
            schema.get("additionalProperties"), False,
            "the verdict record is closed, which is why the outcome is reported beside it",
        )
        items = schema["properties"]["criteria"]["items"]
        self.assertNotIn(
            "additionalProperties", items,
            "a finding must keep accepting the restoration declaration; closing this would "
            "invalidate stored verdicts that carry one",
        )


if __name__ == "__main__":
    unittest.main()
