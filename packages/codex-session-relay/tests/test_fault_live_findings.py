"""CRW-205 live findings (criterion 1): what the installed collector missed on a real run.

A managed start whose creation the host answered without publishing a child never reached the
fault ledger: managed.ManagedStart records a receipt only after an accepted creation, and the
collector read only recorded receipts. And every collected incident stated its installed revision
as unknown, although the runtime installer records the revision it installed from.
"""

import json
import os
from pathlib import Path

from codex_session_relay import faults, faultsweep
from codex_session_relay.managed import operation_ids

from .support import RelayTestCase
from .test_managed_start import ManagedStartFixture

PRODUCT = "crw"
ISSUE = "REL-MANAGED"


class ManagedStartFailuresReachTheLedger(ManagedStartFixture):
    """The creation answer a managed start journals is what the collector reads."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.create_request = operation_ids(self.request["requestId"])[0]

    def swept(self):
        batch = faultsweep.sweep(self.store)
        faultsweep.record_all(self.ledger, batch, store=self.store)
        return batch

    @staticmethod
    def managed(batch, *, cleared=False):
        """Active observations, or with cleared=True the clears the sweep derived."""
        entries = batch["clears"] if cleared else batch["observations"]
        return [entry for entry in entries
                if entry["faultClass"] == "managed_start_failed" and entry["cleared"] == cleared]

    def fault(self, status):
        return self.ledger.get(faults.fault_id(
            PRODUCT, "managed_start_failed", {"issueKey": ISSUE, "receiptStatus": status}))

    def readiness_after(self, ready_calls):
        """Ready for the first n readings, then a worker that cannot take the pair."""
        calls = []

        def observe():
            calls.append(1)
            if len(calls) <= ready_calls:
                return {"observed": True, "policy": self.observation["policy"]}
            return {"observed": False, "reason": "worker_policy_unconfigured"}
        self.start.worker_observation = observe

    def test_a_partial_creation_is_collected_as_a_broken_fault(self):
        self.partial_creation(attempted_turn=True)
        result = self.start.run(self.request)
        self.assertEqual((result["state"], result["stage"], result["reason"]),
                         ("incomplete", "creation", "creation_failed"), result)
        batch = self.swept()
        found = self.managed(batch)
        self.assertEqual(1, len(found), batch["observations"])
        self.assertEqual({"issueKey": ISSUE, "receiptStatus": "failed"}, found[0]["signature"])
        self.assertEqual("managed:managed-1:failed", found[0]["occurrenceKey"])
        self.assertEqual(faults.BROKEN, found[0]["severity"])
        self.assertTrue(any(str(item.get("ref", "")).startswith("journal:")
                            for item in found[0]["evidence"]), found[0]["evidence"])
        self.assertEqual(faults.OPEN, self.fault("failed")["state"],
                         "a broken managed start files at once")

    def test_an_unknown_creation_is_its_own_fault(self):
        self.host.creation_status = "unknown"
        result = self.start.run(self.request)
        self.assertEqual(result["reason"], "creation_unknown", result)
        found = self.managed(self.swept())
        self.assertEqual([{"issueKey": ISSUE, "receiptStatus": "unknown"}],
                         [entry["signature"] for entry in found])
        self.assertNotEqual(
            faults.fault_id(PRODUCT, "managed_start_failed",
                            {"issueKey": ISSUE, "receiptStatus": "unknown"}),
            faults.fault_id(PRODUCT, "managed_start_failed",
                            {"issueKey": ISSUE, "receiptStatus": "failed"}))

    def test_a_start_still_creating_is_not_a_fault(self):
        seen = []
        real_create = self.host.create_thread

        def create(request, **kwargs):
            # The request is armed and the host has not answered: exactly a start in progress.
            seen.append(self.managed(faultsweep.sweep(self.store)))
            return real_create(request, **kwargs)
        self.host.create_thread = create
        self.assertEqual(self.start.run(self.request)["state"], "admitted")
        self.assertEqual([[]], seen)
        self.assertEqual([], self.managed(self.swept()))

    def test_a_readiness_refusal_before_the_host_is_asked_is_not_a_fault(self):
        self.readiness_after(1)
        result = self.start.run(self.request)
        self.assertEqual((result["state"], result["stage"]), ("refused", "creation"), result)
        self.assertEqual([], self.managed(self.swept()))

    def test_a_later_creation_stage_refusal_supersedes_the_answer_and_clears_it(self):
        self.host.creation_status = "unknown"
        self.start.run(self.request)
        self.assertEqual(1, len(self.managed(self.swept())))
        # The bridge holds no receipt for the attempt, so the retry reaches the readiness check
        # at creation again, and this time the worker cannot take the pair: the host is not
        # asked, and the newest creation-stage answer is that refusal.
        self.host.operations.pop(self.create_request)
        self.readiness_after(1)
        retried = self.start.run(self.request)
        self.assertEqual((retried["state"], retried["stage"]), ("refused", "creation"), retried)
        batch = self.swept()
        self.assertEqual([], self.managed(batch))
        self.assertEqual(1, len(self.managed(batch, cleared=True)))
        self.assertIsNotNone(self.fault("unknown")["cleared_at"])

    def test_the_same_request_attaching_later_clears_it(self):
        self.host.creation_status = "unknown"
        self.start.run(self.request)
        self.assertEqual(1, len(self.managed(self.swept())))
        self.host.operations.pop(self.create_request)
        self.host.creation_status = "accepted"
        self.assertEqual(self.start.run(self.request)["state"], "admitted")
        batch = self.swept()
        self.assertEqual([], self.managed(batch))
        self.assertEqual(1, len(self.managed(batch, cleared=True)))
        self.assertIsNotNone(self.fault("unknown")["cleared_at"])

    def test_a_later_preflight_refusal_does_not_hide_the_failure(self):
        self.partial_creation(attempted_turn=True)
        self.start.run(self.request)
        self.assertEqual(1, len(self.managed(self.swept())))
        # A retry that stops before creation says nothing about what the host answered.
        self.readiness_after(0)
        refused = self.start.run(self.request)
        self.assertEqual(refused["stage"], "preflight", refused)
        batch = self.swept()
        self.assertEqual([], self.managed(batch, cleared=True))
        self.assertIsNone(self.fault("failed")["cleared_at"])


SOURCE = {"repositoryCommit": "a" * 40, "repositoryTree": "b" * 40,
          "subdirectoryTree": "c" * 40, "workingTreeClean": True}


class OccurrencesStateTheInstalledRevision(RelayTestCase):
    """The revision comes from this copy's own entry in the installer's host record."""

    def setUp(self):
        super().setUp()
        self.package = str(Path(faultsweep.__file__).resolve().parent)
        self.path = (Path(os.environ["XDG_STATE_HOME"]) / "codex-relay-workflow"
                     / "host-record.json")

    def record(self, installs, *, version=1, component_facts=None, raw=None):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        if raw is not None:
            self.path.write_text(raw, encoding="utf-8")
            return
        component = {"installs": installs, "measuredPoints": [], **(component_facts or {})}
        self.path.write_text(json.dumps({
            "recordVersion": version, "definitionVersion": 1,
            "components": {"codex-session-relay": component}}), encoding="utf-8")

    @staticmethod
    def facts():
        return faultsweep._facts(expected="e", actual="a", impact="i")["observed"]

    def test_this_copys_install_entry_states_its_revision(self):
        self.record([
            {"location": "/elsewhere/codex_session_relay", "source": dict(SOURCE, repositoryCommit="e" * 40)},
            {"location": self.package, "environment": "/env", "integrity": "d" * 64,
             "source": SOURCE},
        ])
        facts = self.facts()
        revision = facts["installation"].get("revision")
        self.assertIsNotNone(revision, facts["installation"])
        self.assertEqual("a" * 40, revision["repositoryCommit"])
        self.assertEqual("c" * 40, revision["subdirectoryTree"])
        self.assertEqual(str(self.path), facts["installation"].get("revisionRecord"))
        self.assertNotIn(faultsweep.INSTALLATION_LIMIT, facts["limits"])

    def test_a_collected_incident_carries_the_revision(self):
        self.record([{"location": self.package, "source": SOURCE}])
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO managed_start_requests (request_id, issue_key,"
                " request_fingerprint, fingerprint_version, workspace, marker_root,"
                " socket_identity, create_request_id, dispatch_request_id, state, revision,"
                " receipt_status, created_at, updated_at)"
                " VALUES ('req-1', 'CRW-9', 'fp', 'v1', '/w', '/m', 'sock', 'c', 'd',"
                " 'create_armed', 2, 'failed', 't', 't')")
        found = [entry for entry in faultsweep.sweep(self.store)["observations"]
                 if entry["faultClass"] == "managed_start_failed"]
        facts = [item["observed"] for item in found[0]["evidence"] if item["kind"] == "facts"]
        revision = facts[0]["installation"].get("revision")
        self.assertIsNotNone(revision, facts[0]["installation"])
        self.assertEqual("a" * 40, revision["repositoryCommit"])

    def test_component_facts_a_rollback_left_behind_are_never_read(self):
        # A failed install's rollback removes its entry and leaves the component-level facts
        # it wrote; the entry left last is an older install that recorded no revision.
        self.record([{"location": self.package, "environment": "/older"}],
                    component_facts={"repositoryCommit": "f" * 40,
                                     "subdirectoryTree": "f" * 40})
        facts = self.facts()
        self.assertIsNone(facts["installation"].get("revision"))
        self.assertIn("records no revision", facts["installation"].get("revisionReason") or "")
        self.assertIn(faultsweep.INSTALLATION_LIMIT, facts["limits"])

    def test_another_locations_revision_is_not_this_copys(self):
        self.record([{"location": "/elsewhere/codex_session_relay", "source": SOURCE}])
        facts = self.facts()
        self.assertIsNone(facts["installation"].get("revision"))
        self.assertIn(self.package, facts["installation"].get("revisionReason") or "")

    def test_an_absent_unreadable_or_foreign_record_leaves_it_unknown(self):
        cases = {
            "absent": None,
            "unreadable": "{not json",
            "another record version": json.dumps({"recordVersion": 2, "components": {}}),
            "a malformed source": json.dumps({"recordVersion": 1, "components": {
                "codex-session-relay": {"installs": [
                    {"location": self.package, "source": {"repositoryCommit": 7}}]}}}),
        }
        for name, raw in cases.items():
            with self.subTest(name):
                if raw is None:
                    if self.path.exists():
                        self.path.unlink()
                else:
                    self.record(None, raw=raw)
                facts = self.facts()
                self.assertIsNone(facts["installation"].get("revision"))
                self.assertTrue(facts["installation"].get("revisionReason") or "")
                self.assertIn(faultsweep.INSTALLATION_LIMIT, facts["limits"])
