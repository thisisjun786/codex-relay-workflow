"""The corrected fault-ledger contract, one class per post-merge blocker and per audit finding.

Every test here names the defect it guards and fails with an AssertionError on the merged bytes
(dev 3a40df1c). Nothing touches a network: the connector's side is played by handing the ledger
the text or fields a connector would have read back.
"""

import inspect
import json

from codex_session_relay import faults, faultsweep
from codex_session_relay.errors import RefusalReason

from .support import RelayTestCase

PRODUCT = "crw"
TEAM = "team-relay"
PROJECT = "proj-crw"
SCOPE = {"projectKey": "CRW", "issueKey": "CRW-205"}


def observation(key, *, fault_class="report_omitted", signature=None, severity=faults.BROKEN,
                scope=None, cleared=False, product=PRODUCT, detail="observed", evidence=None):
    return faults.observation(
        product=product, fault_class=fault_class, severity=severity,
        signature=signature or {"relationship": "rel-1", "turn": "turn-7"},
        occurrence_key=key, scope=dict(SCOPE if scope is None else scope), cleared=cleared,
        detail=detail,
        evidence=evidence if evidence is not None else [{"kind": "row", "ref": "events"}],
    )


def capability(case, owner, name):
    """The capability the contract names, or a failure saying which one is missing."""
    found = getattr(owner, name, None)
    case.assertTrue(callable(found), f"the ledger offers no {name}()")
    return found


class ContractCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref=PROJECT)

    def created(self, publication, *, owner="writer-A"):
        claim = self.ledger.claim(publication, owner=owner)
        operation = self.ledger.operation(publication, claim_token=claim["claimToken"])
        return claim, operation

    def publish(self, publication, *, external_ref="REL-1", project_ref=PROJECT, owner="writer-A"):
        claim, operation = self.created(publication, owner=owner)
        return self.ledger.complete(
            publication, claim_token=claim["claimToken"],
            readback="issue body\n\n" + operation["block"] + "\n", external_ref=external_ref,
            project_ref=project_ref,
        )

    def opened(self, key="k1", **kw):
        first = self.ledger.record(observation(key, **kw))
        self.assertIsNotNone(first["publication"], "a broken fault opens a record")
        return first["faultId"], first["publication"]["publicationId"]


class B1_ALiveIssuedCreateIsNeverReissued(ContractCase):
    """Post-merge blocker 1: an attested absence released a live issued create to a second writer."""

    def test_an_absence_under_a_live_lease_changes_nothing(self):
        _, pub = self.opened()
        self.created(pub, owner="writer-A")
        answer = self.ledger.reconcile(pub, "", searched=True)
        self.assertEqual(answer["outcome"], "absent_in_flight")
        self.assertEqual(answer["state"], faults.ISSUED)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.claim(pub, owner="writer-B")

    def test_a_lapsed_create_stays_uncertain_without_an_attested_end(self):
        _, pub = self.opened()
        self.created(pub)
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.ledger.expire_leases()
        answer = self.ledger.reconcile(pub, "", searched=True)
        self.assertEqual(answer["outcome"], "absent_unproven")
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["state"], faults.UNCERTAIN)
        freed = self.ledger.reconcile(pub, "", searched=True, prior_ended=True,
                                      reason="the connector refused the request with a 400")
        self.assertEqual(freed["state"], faults.PENDING)

    def test_a_plain_failure_after_issue_proves_no_end(self):
        _, pub = self.opened()
        claim, _ = self.created(pub)
        self.ledger.fail(pub, claim_token=claim["claimToken"], error="timeout")
        self.assertEqual(self.ledger.reconcile(pub, "", searched=True)["outcome"],
                         "absent_unproven")

    def test_a_definitive_refusal_reported_by_the_holder_is_an_end(self):
        _, pub = self.opened()
        claim, _ = self.created(pub)
        self.ledger.fail(pub, claim_token=claim["claimToken"], error="400 invalid", ended=True)
        self.assertEqual(self.ledger.reconcile(pub, "", searched=True)["state"], faults.PENDING)


class B2_EveryRotationReachesTheEnd(RelayTestCase):
    """Post-merge blocker 2: the cursor wrapped after four full pages and starved the rest."""

    def seed(self, count):
        with self.store.transaction() as db:
            for n in range(count):
                db.execute(
                    "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                    " recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                    " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (f"e{n:02d}", "r", "completion", f"p{n}", "p", "withheld_pre_send", 6,
                     "attempt_cap", "t", "t"))

    def test_a_ten_row_source_is_read_to_its_last_row(self):
        self.seed(10)
        seen = set()
        for _ in range(6):
            batch = faultsweep.sweep(self.store, limit=2)
            seen |= {entry["occurrenceKey"] for entry in batch["observations"]
                     if entry["faultClass"] == "delivery_stalled"}
            faultsweep.write_cursors(self.store, batch["_advanced"])
        self.assertEqual(seen, {f"delivery:e{n:02d}" for n in range(10)})

    def test_a_row_added_behind_the_cursor_is_read_by_the_next_rotation(self):
        self.seed(6)
        seen = set()
        for tick in range(8):
            if tick == 2:
                with self.store.transaction() as db:
                    db.execute(
                        "INSERT INTO deliveries (event_id, relationship_id, kind,"
                        " recipient_task_id, recipient_thread_id, state, attempt_count,"
                        " hold_reason, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                        ("e00a", "r", "completion", "late", "p", "withheld_pre_send", 6,
                         "attempt_cap", "t", "t"))
            batch = faultsweep.sweep(self.store, limit=2)
            seen |= {entry["occurrenceKey"] for entry in batch["observations"]
                     if entry["faultClass"] == "delivery_stalled"}
            faultsweep.write_cursors(self.store, batch["_advanced"])
        self.assertIn("delivery:e00a", seen)
        self.assertIn("delivery:e05", seen)


class B3_ACauseThatComesBackIsSeen(ContractCase):
    """Post-merge blocker 3 and plan-audit round 1 #2: the same key after a clear was dropped."""

    def test_the_same_key_after_a_clear_blocks_resolution(self):
        identifier, pub = self.opened("same")
        self.publish(pub)
        self.ledger.record(observation("same", cleared=True))
        self.ledger.record_fix(identifier, ref="fix-1")
        self.ledger.record_reverification(identifier, method="suite", ref="check-1",
                                          outcome="passed")
        again = self.ledger.record(observation("same"))
        self.assertTrue(again["recorded"], "the recurrence was dropped as already recorded")
        self.assertEqual(again["state"], faults.OPEN)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.resolve(identifier)
        self.assertEqual(self.ledger.get(identifier)["state"], faults.OPEN)

    def test_any_key_after_a_clear_opens_the_episode(self):
        identifier, pub = self.opened("A")
        self.publish(pub)
        self.ledger.record(observation("A", cleared=True))
        self.ledger.record(observation("B"))
        self.ledger.record_fix(identifier, ref="fix-1")
        self.ledger.record_reverification(identifier, method="suite", ref="check-1",
                                          outcome="passed")
        self.assertTrue(self.ledger.record(observation("A"))["recorded"])
        with self.assertRaises(faults.FaultRefused):
            self.ledger.resolve(identifier)

    def test_a_repeated_sweep_after_the_recurrence_records_nothing(self):
        _, pub = self.opened("same")
        self.publish(pub)
        self.ledger.record(observation("same", cleared=True))
        self.assertTrue(self.ledger.record(observation("same"))["recorded"])
        self.assertFalse(self.ledger.record(observation("same"))["recorded"])

    def test_a_clear_under_the_active_key_keeps_both_evidence_rows(self):
        identifier, pub = self.opened("same")
        self.publish(pub)
        self.ledger.record(observation("same", cleared=True))
        rows = self.store.all("SELECT cleared FROM fault_occurrences WHERE fault_id = ?",
                              (identifier,))
        self.assertEqual(sorted(row["cleared"] for row in rows), [0, 1])


class B4_AnIssueIsFiledInItsProject(ContractCase):
    """Post-merge blocker 4: no project id anywhere, so a project-less create confirmed."""

    def test_the_operation_names_the_project(self):
        _, pub = self.opened()
        _, operation = self.created(pub)
        self.assertEqual(operation.get("projectRef"), PROJECT)

    def test_a_create_without_a_project_readback_is_refused(self):
        _, pub = self.opened()
        claim, operation = self.created(pub)
        with self.assertRaises(faults.FaultRefused) as refused:
            self.ledger.complete(pub, claim_token=claim["claimToken"],
                                 readback=operation["block"], external_ref="REL-1")
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_READBACK_MISMATCH)

    def test_a_create_landing_elsewhere_is_repaired_on_the_same_issue(self):
        identifier, pub = self.opened()
        self.publish(pub, project_ref="proj-elsewhere")
        fault = self.ledger.get(identifier)
        self.assertEqual(fault["external_ref"], "REL-1")
        self.assertEqual(fault.get("linkState"), "unlinked")
        updates = capability(self, self.ledger, "publications")(identifier, kind="update_record")
        self.assertEqual([entry["payload"]["op"] for entry in updates], ["set_project"])
        self.assertEqual(updates[0]["payload"]["value"], PROJECT)

    def test_a_scope_without_a_project_never_creates(self):
        self.ledger.set_target(product=PRODUCT, project="OPS", team=TEAM, project_ref=None)
        _, pub = self.opened(scope={"projectKey": "OPS"})
        self.assertNotIn(pub, [entry["publication_id"] for entry in self.ledger.next(limit=10)])
        with self.assertRaises(faults.FaultRefused):
            self.ledger.claim(pub, owner="writer-A")

    def test_a_target_changed_between_claim_and_operation_is_never_used(self):
        _, pub = self.opened()
        claim = self.ledger.claim(pub, owner="writer-A")
        self.ledger.set_target(product=PRODUCT, project="CRW", team="team-new",
                               project_ref="proj-new")
        with self.assertRaises(faults.FaultRefused):
            self.ledger.operation(pub, claim_token=claim["claimToken"])
        row = capability(self, self.ledger, "publication")(pub)
        self.assertEqual((row["state"], row["tracker_ref"], row["target"]["projectRef"]),
                         (faults.PENDING, "team-new", "proj-new"))
        self.assertEqual(row["attempts"], 0)


class B5_TheSupportedBoundariesAreCollected(RelayTestCase):
    """Post-merge blocker 5: settings refusals and failed managed starts were never collected."""

    def withhold(self, event, reason, *, relationship="rel-1"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR IGNORE INTO deliveries (event_id, relationship_id, kind,"
                " recipient_task_id, recipient_thread_id, state, attempt_count, created_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,?)",
                (event, relationship, "completion", "parent", "parent", "withheld_pre_send", 0,
                 "t", "t"))
            self.store.journal("delivery_withheld", event, {"reason": reason, "detail": "no"},
                               at="t")

    def test_three_settings_refusals_make_one_filed_fault(self):
        self.assertIn("delivery_refused", faults.CLASS_POLICY)
        ledger = faults.FaultLedger(self.store, self.clock)
        for _ in range(3):
            self.withhold("e1", "settings_incomplete")
        batch = faultsweep.sweep(self.store)
        refused = [entry for entry in batch["observations"]
                   if entry["faultClass"] == "delivery_refused"]
        self.assertEqual(len(refused), 3)
        results = [ledger.record(entry) for entry in refused]
        self.assertEqual(len({entry["faultId"] for entry in results}), 1)
        self.assertEqual(results[-1]["state"], faults.OPEN)

    def test_a_paused_recipient_is_never_a_fault(self):
        for _ in range(3):
            self.withhold("e1", "paused")
        batch = faultsweep.sweep(self.store)
        self.assertEqual([entry for entry in batch["observations"]
                          if entry["faultClass"] == "delivery_refused"], [])

    def test_a_failed_managed_start_is_collected(self):
        self.assertIn("managed_start_failed", faults.CLASS_POLICY)
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO managed_start_requests (request_id, issue_key,"
                " request_fingerprint, fingerprint_version, workspace, marker_root,"
                " socket_identity, create_request_id, dispatch_request_id, state, revision,"
                " receipt_status, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                ("req-1", "CRW-9", "fp", "v1", "/w", "/m", "sock", "create-1", "dispatch-1",
                 "create_armed", 2, "failed", "t", "t"))
        batch = faultsweep.sweep(self.store)
        self.assertEqual([entry["faultClass"] for entry in batch["observations"]
                          if entry["faultClass"] == "managed_start_failed"],
                         ["managed_start_failed"])


class B5_ARefusalStreakEndsOnlyWhereTheContractSays(RelayTestCase):
    """Post-merge blocker 5, continued: only a delivery's CURRENT refusal streak counts.

    A streak ends on a settled send, on a person pausing the work, on a withholding for another
    reason, or on the delivery settling - and never on a busy deferral, which says nothing about
    the settings that were refused.
    """

    def setUp(self):
        super().setUp()
        self.assertIn("delivery_refused", faults.CLASS_POLICY, "refusals are not collected")

    def journal(self, kind, detail="", *, event="e1"):
        with self.store.transaction():
            self.store.journal(kind, event, detail, at="t")

    def refuse(self, reason="settings_incomplete", *, event="e1"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR IGNORE INTO deliveries (event_id, relationship_id, kind,"
                " recipient_task_id, recipient_thread_id, state, attempt_count, created_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,?)",
                (event, "rel-1", "completion", "parent", "parent", "withheld_pre_send", 0,
                 "t", "t"))
        self.journal("delivery_withheld", {"reason": reason, "detail": "no"}, event=event)

    def refused(self, batch=None):
        batch = batch if batch is not None else faultsweep.sweep(self.store)
        return [entry for entry in batch["observations"]
                if entry["faultClass"] == "delivery_refused"]

    def test_a_settled_send_between_refusals_ends_the_first_streak(self):
        self.refuse()
        self.refuse()
        self.journal("delivery_attempted", {"requestId": "r1", "state": "withheld_pre_send"})
        self.refuse()
        self.refuse()
        self.assertEqual(2, len(self.refused()))

    def test_a_busy_deferral_between_refusals_ends_nothing(self):
        self.refuse()
        self.journal("delivery_deferred_busy")
        self.refuse()
        self.refuse()
        self.assertEqual(3, len(self.refused()))

    def test_a_person_pausing_the_assignment_ends_the_streak(self):
        for _ in range(3):
            self.refuse()
        self.journal("delivery_withheld_inactive", {"relationshipId": "rel-1", "status": "paused"})
        self.assertEqual([], self.refused())

    def test_a_delivery_that_settled_has_no_streak(self):
        for _ in range(3):
            self.refuse()
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET state = 'dispatched' WHERE event_id = 'e1'")
        self.assertEqual([], self.refused())

    def test_another_reason_ends_the_streak_and_clears_the_fault(self):
        ledger = faults.FaultLedger(self.store, self.clock)
        for _ in range(3):
            self.refuse()
        faultsweep.record_all(ledger, faultsweep.sweep(self.store), store=self.store)
        identifier = faults.fault_id(PRODUCT, "delivery_refused",
                                     {"relationship": "rel-1", "errorCode": "settings_incomplete"})
        self.assertEqual(faults.OPEN, ledger.get(identifier)["state"])
        self.refuse("unsupported_sandbox_type")
        batch = faultsweep.sweep(self.store)
        self.assertEqual(["unsupported_sandbox_type"],
                         [entry["signature"]["errorCode"] for entry in self.refused(batch)])
        self.assertIn(identifier, [faults.fault_id(PRODUCT, entry["faultClass"],
                                                   entry["signature"])
                                   for entry in batch["clears"]])


class B5_AFailedManagedStartClearsWhenAccepted(RelayTestCase):
    """The registry replaces a non-publishing receipt; an accepted one is the only way out."""

    def test_an_accepted_receipt_clears_the_failed_start(self):
        self.assertIn("managed_start_failed", faults.CLASS_POLICY)
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO managed_start_requests (request_id, issue_key,"
                " request_fingerprint, fingerprint_version, workspace, marker_root,"
                " socket_identity, create_request_id, dispatch_request_id, state, revision,"
                " receipt_status, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                ("req-1", "CRW-9", "fp", "v1", "/w", "/m", "sock", "create-1", "dispatch-1",
                 "create_armed", 2, "failed", "t", "t"))
        ledger = faults.FaultLedger(self.store, self.clock)
        faultsweep.record_all(ledger, faultsweep.sweep(self.store), store=self.store)
        identifier = faults.fault_id(PRODUCT, "managed_start_failed", {"issueKey": "CRW-9"})
        self.assertEqual(faults.OPEN, ledger.get(identifier)["state"])
        with self.store.transaction() as db:
            db.execute("UPDATE managed_start_requests SET receipt_status = 'accepted'")
        batch = faultsweep.sweep(self.store)
        self.assertIn(identifier, [faults.fault_id(PRODUCT, entry["faultClass"],
                                                   entry["signature"])
                                   for entry in batch["clears"]])


class B5_ManagedTurnsAreReadThroughTheProjection(RelayTestCase):
    """The relay's own managed turns are read through omitted.observe, which is consumed as
    reporting-show consumes it: the selection, the marker root, the workspace, the hashed
    assignment, the child session and the turn. Nothing else supplies CRW-180 readings to a
    daemon, so without this the omission class only ever saw what an operator typed in."""

    def setUp(self):
        from .support import CHILD

        super().setUp()
        self.assertIn("selection", inspect.signature(faultsweep.sweep).parameters,
                      "the sweep cannot read the relay's own managed turns")
        self.register()
        row = self.store.one("SELECT relationship_id, execution_generation FROM relationships")
        self.relationship = row["relationship_id"]
        self.child = CHILD
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO managed_start_requests (request_id, issue_key,"
                " request_fingerprint, fingerprint_version, workspace, marker_root,"
                " socket_identity, create_request_id, dispatch_request_id, state, revision,"
                " child_task_id, standby_turn_id, relationship_id, execution_generation,"
                " receipt_status, created_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                ("req-1", "CRW-9", "fp", "v1", "/w", "/m", "sock", "create-1", "dispatch-1",
                 "attached", 3, CHILD, "standby-1", self.relationship,
                 row["execution_generation"], "accepted", "t", "t"))
            for turn, status in (("turn-9", "completed"), ("turn-9", "failed"),
                                 ("standby-1", "completed")):
                db.execute("INSERT INTO assignment_settlements VALUES (?,?,?,?,?)",
                           (self.relationship, CHILD, turn, status, "t"))

    def observed(self, answer):
        from unittest import mock

        from codex_session_relay import omitted

        calls = []

        def observe(selection, **kw):
            calls.append({"selection": selection, **kw})
            if isinstance(answer, Exception):
                raise answer
            return {"schema": faultsweep.OBSERVATION_SCHEMA, "reportingState": answer,
                    "relationshipId": self.relationship,
                    "selectors": {"turn": kw["turn"], "session": kw["session"]}}

        with mock.patch.object(omitted, "observe", observe):
            batch = faultsweep.sweep(self.store, selection="the-selection", now="t")
        return calls, batch

    def test_each_settled_turn_is_read_once_and_the_standby_turn_never(self):
        from codex_session_relay import marker

        calls, batch = self.observed("unreported")
        self.assertEqual([{"selection": "the-selection", "root": "/m", "workspace": "/w",
                           "assignment": marker.assignment_id("dispatch-1"),
                           "session": self.child, "turn": "turn-9", "now": "t"}], calls)
        self.assertEqual([{"relationship": self.relationship, "turn": "turn-9"}],
                         [entry["signature"] for entry in batch["observations"]
                          if entry["faultClass"] == "report_omitted" and not entry["cleared"]])

    def test_an_observer_error_is_a_gap_and_the_sweep_goes_on(self):
        calls, batch = self.observed(OSError("marker unreadable"))
        self.assertEqual(1, len(calls))
        self.assertEqual(["managed_reading_failed"], [gap["gap"] for gap in batch["gaps"]])

    def test_a_relationship_past_its_managed_generation_is_not_read(self):
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET execution_generation = execution_generation + 1")
        calls, _batch = self.observed("unreported")
        self.assertEqual([], calls)


class SupersededDeliveriesAreNeverCurrentFaults(RelayTestCase):
    """Routed from CRW-214's final reviews: a delivery whose obligation was superseded stayed a
    current broken fault that never cleared.

    A delivery parked at its attempt cap cannot be rewritten to superseded, so the relay
    annotates it instead, and the rest of supersession (a later generation, a newer revision, a
    regranted merge turn) is decided live. The sweep read neither, for any delivery kind.
    """

    def held(self, event, recipient, *, relationship="rel-1"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                " recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event, relationship, "completion", recipient, recipient, "withheld_pre_send",
                 6, "attempt_cap", "t", "t"))

    def stalled(self, batch):
        return {entry["signature"]["recipient"] for entry in batch["observations"]
                if entry["faultClass"] == "delivery_stalled"}

    def test_an_annotated_capped_delivery_clears_and_a_live_one_still_records(self):
        ledger = faults.FaultLedger(self.store, self.clock)
        self.held("e1", "p1")
        self.held("e2", "p2")
        faultsweep.record_all(ledger, faultsweep.sweep(self.store), store=self.store)
        overtaken = faults.fault_id(PRODUCT, "delivery_stalled",
                                    {"recipient": "p1", "attemptState": None})
        self.assertEqual(faults.OPEN, ledger.get(overtaken)["state"])
        with self.store.transaction() as db:
            db.execute("INSERT INTO delivery_supersession (event_id, reason, noted_at, applied)"
                       " VALUES ('e1', 'superseded_revision', 't', 0)")
        batch = faultsweep.sweep(self.store)
        self.assertEqual({"p2"}, self.stalled(batch))
        self.assertIn(overtaken, [faults.fault_id(PRODUCT, entry["faultClass"],
                                                  entry["signature"])
                                  for entry in batch["clears"]])

    def test_a_delivery_a_later_generation_overtook_is_never_recorded(self):
        self.register()
        relationship = self.store.one("SELECT relationship_id FROM relationships")[
            "relationship_id"]
        with self.store.transaction() as db:
            for event, generation in (("old", 1), ("new", 2)):
                db.execute(
                    "INSERT INTO events (event_id, relationship_id, execution_generation,"
                    " revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status,"
                    " receipt, first_seen_at, last_seen_at)"
                    " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                    (event, relationship, generation, "hash-" + event, "ready_for_review",
                     "child", "child-thread", "turn-" + event, "completed", "{}", "t", "t"))
            db.execute("UPDATE relationships SET execution_generation = 2")
        self.held("old", "p-old", relationship=relationship)
        self.held("new", "p-new", relationship=relationship)
        self.assertEqual({"p-new"}, self.stalled(faultsweep.sweep(self.store)))

    def test_a_replaced_relationship_neither_retries_nor_refuses(self):
        self.register()
        relationship = self.store.one("SELECT relationship_id FROM relationships")[
            "relationship_id"]
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET superseded_by = 'rel-successor'")
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                " recipient_thread_id, state, attempt_count, created_at, updated_at)"
                " VALUES ('e9', ?, 'completion', 'p9', 'p9', 'withheld_pre_send', 2, 't', 't')",
                (relationship,))
            for number in (1, 2):
                db.execute(
                    "INSERT INTO attempts (request_id, event_id, attempt_no, kind,"
                    " internal_state, state, observed_at) VALUES (?,?,?,?,?,?,?)",
                    (f"req-{number}", "e9", number, "completion", "settled",
                     "withheld_pre_send", "t"))
            for _ in range(3):
                self.store.journal("delivery_withheld", "e9",
                                   {"reason": "settings_incomplete", "detail": "no"}, at="t")
        batch = faultsweep.sweep(self.store)
        self.assertEqual([], [entry["faultClass"] for entry in batch["observations"]
                              if entry["faultClass"] in ("delivery_stalled",
                                                         "delivery_refused")])


class AnAnchorTheSchedulerNoLongerReadsIsNeverStalled(RelayTestCase):
    """The same class as superseded deliveries, on the anchor source: the scheduler reads only
    the current generation of an active relationship nobody replaced. A failed last poll on a
    paused assignment (waiting) or on a generation it moved past (overtaken) is never read
    again, so collecting it made a stalled fault that nothing could clear."""

    def setUp(self):
        super().setUp()
        self.register()
        row = self.store.one("SELECT relationship_id, execution_generation FROM relationships")
        self.relationship, self.generation = row["relationship_id"], row["execution_generation"]
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR REPLACE INTO generations (relationship_id, execution_generation,"
                " dispatch_request_id, anchor_state, dispatch_turn_id, opened_at)"
                " VALUES (?,?,?,?,?,?)",
                (self.relationship, self.generation, "req-anchor", "bound", "turn-a", "t"))
            db.execute(
                "INSERT OR REPLACE INTO poll_observations (relationship_id,"
                " execution_generation, turn_id, last_status, last_polled_at, last_attempt_at,"
                " last_error) VALUES (?,?,?,?,?,?,?)",
                (self.relationship, self.generation, "turn-a", "failed", None, "t", "boom"))

    def stalled(self):
        return [entry for entry in faultsweep.sweep(self.store)["observations"]
                if entry["faultClass"] == "observation_stalled"]

    def test_a_failed_poll_of_the_current_active_anchor_still_records(self):
        self.assertEqual(1, len(self.stalled()))

    def test_a_paused_assignment_is_waiting_not_stalled(self):
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET status = 'paused'")
        self.assertEqual([], self.stalled())

    def test_a_generation_the_assignment_moved_past_is_not_stalled(self):
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET execution_generation = execution_generation + 1")
        self.assertEqual([], self.stalled())


class B6_WorkspaceIsPartOfIdentity(ContractCase):
    """Post-merge blocker 6: two workspaces merged into one fault and overwrote each other."""

    def test_two_workspaces_are_two_faults(self):
        a = self.ledger.record(observation("a", scope={"workspace": "ws-A", "projectKey": "CRW"}))
        b = self.ledger.record(observation("b", scope={"workspace": "ws-B", "projectKey": "CRW"}))
        self.assertNotEqual(a["faultId"], b["faultId"])
        self.assertEqual(json.loads(self.ledger.get(a["faultId"])["scope"])["workspace"], "ws-A")

    def test_a_project_move_inside_one_workspace_is_one_fault(self):
        a = self.ledger.record(observation("a", scope={"workspace": "ws-A", "projectKey": "CRW"}))
        b = self.ledger.record(observation("b", scope={"workspace": "ws-A", "projectKey": "OPS"}))
        self.assertEqual(a["faultId"], b["faultId"])

    def test_the_id_is_computable_before_the_first_record(self):
        self.assertIn("workspace", inspect.signature(faults.fault_id).parameters)
        computed = faults.fault_id(PRODUCT, "report_omitted",
                                   {"relationship": "rel-1", "turn": "turn-7"}, workspace="ws-A")
        recorded = self.ledger.record(
            observation("a", scope={"workspace": "ws-A", "projectKey": "CRW"}))
        self.assertEqual(computed, recorded["faultId"])

    def test_an_unassigned_fault_moves_out_and_keeps_its_record(self):
        self.assertEqual(getattr(faults, "UNASSIGNED", None), "unassigned")
        first = self.ledger.record(
            observation("a", scope={"workspace": "unassigned", "projectKey": "CRW"}))
        capability(self, self.ledger, "move")(
            first["faultId"], scope={"workspace": "ws-A", "projectKey": "CRW"})
        later = self.ledger.record(
            observation("b", scope={"workspace": "ws-A", "projectKey": "CRW"}))
        self.assertEqual(later["faultId"], first["faultId"])
        stale = self.ledger.record(
            observation("c", scope={"workspace": "unassigned", "projectKey": "CRW"}))
        self.assertEqual(stale["faultId"], first["faultId"])
        self.assertEqual(json.loads(self.ledger.get(first["faultId"])["scope"])["workspace"],
                         "ws-A")


class B7_BudgetsHoldAndNeverStarveAnotherProduct(ContractCase):
    """Post-merge blocker 7 and CRW-206 7a: no per-product limit, and no fairness."""

    def test_one_product_is_held_at_its_create_budget(self):
        for n in range(8):
            self.ledger.record(observation(f"k{n}", signature={"relationship": f"r{n}",
                                                                "turn": "t"}))
        offered = self.ledger.next(limit=50)
        self.assertEqual(len(offered), 5)
        state = capability(self, self.ledger, "queue_state")(limit=50)
        self.assertEqual(len(state["held"]), 3)
        self.assertTrue(all(entry["reason"] == "budget_spent" for entry in state["held"]))

    def test_a_capped_product_does_not_hide_another(self):
        self.ledger.set_target(product="lina", project="LINA", team="team-lina",
                               project_ref="proj-lina")
        for n in range(8):
            self.ledger.record(observation(f"k{n}", signature={"relationship": f"r{n}",
                                                                "turn": "t"}))
        for entry in self.ledger.next(limit=5):
            self.ledger.claim(entry["publication_id"], owner="writer")
        self.ledger.record(observation("lina-1", product="lina", scope={"projectKey": "LINA"}))
        products = {self.ledger.get(entry["fault_id"])["product"]
                    for entry in self.ledger.next(limit=3)}
        self.assertIn("lina", products)

    def test_a_caller_can_check_and_consume(self):
        consume = capability(self, self.ledger, "consume")
        for n in range(10):
            self.assertTrue(consume(PRODUCT, "notification", ref=f"n{n}")["consumed"])
        spent = consume(PRODUCT, "notification", ref="n10")
        self.assertEqual((spent["consumed"], spent["reason"]), (False, "budget_spent"))
        self.assertTrue(consume(PRODUCT, "notification", ref="n3")["consumed"],
                        "one ref is consumed once and answers the same")


class B8_ReadingInputIsBounded(RelayTestCase):
    """Post-merge blocker 8: readings bypassed the sweep's bound."""

    def reading(self, n, state="unreported"):
        return {"schema": "reporting-observation/1", "relationshipId": f"r{n}",
                "reportingState": state, "selectors": {"turn": f"t{n}"}}

    def test_a_limit_of_one_reads_one_reading_and_continues(self):
        readings = [self.reading(n) for n in range(10)]
        batch = faultsweep.sweep(self.store, limit=1, readings=readings)
        self.assertLessEqual(len([entry for entry in batch["observations"]
                                  if entry["faultClass"] == "report_omitted"]), 1)
        self.assertEqual(batch.get("readingsNext"), 1)

    def test_a_reading_batch_over_the_ceiling_is_refused(self):
        with self.assertRaises(faults.FaultRefused):
            faultsweep.sweep(self.store, readings=[self.reading(n) for n in range(1001)])

    def test_the_last_reading_of_a_turn_wins_before_paging(self):
        readings = [self.reading(1, "unreported"), self.reading(1, "reported")]
        batch = faultsweep.sweep(self.store, limit=1, readings=readings)
        self.assertEqual([entry["faultClass"] for entry in batch["observations"]
                          if not entry["cleared"]], [])


class B9_NothingMalformedReachesTheStore(ContractCase):
    """Post-merge blocker 9: a list detail reached SQLite and was reported as an outage."""

    def test_a_list_detail_is_refused_before_the_store(self):
        with self.assertRaises(Exception) as caught:
            self.ledger.record(observation("k", detail=["not text"]))
        self.assertIsInstance(caught.exception, faults.FaultRefused,
                              "the store, not the ledger, refused the observation")
        self.assertEqual(caught.exception.reason, RefusalReason.FAULT_OBSERVATION_MALFORMED)
        self.assertEqual(self.store.one("SELECT COUNT(*) AS n FROM fault_ledger")["n"], 0)

    def test_a_product_with_a_separator_is_refused(self):
        with self.assertRaises(faults.FaultRefused):
            self.ledger.record(observation("k", product="a:b"))

    def test_a_batch_isolates_the_refused_observation(self):
        batch = {"observations": [observation("k", detail=["bad"]), observation("k2")]}
        try:
            answer = faultsweep.record_all(self.ledger, batch)
        except Exception as error:  # noqa: BLE001 - the defect is the whole batch dying
            self.fail(f"one malformed observation took the batch down: {error!r}")
        self.assertEqual(answer["recorded"], 1)
        self.assertEqual([gap["gap"] for gap in answer["gaps"]], ["observation_refused"])


class B10_LifecycleStagesAreSeparate(ContractCase):
    """Post-merge blocker 10: acceptance, assignment, merge and installation had no state."""

    def test_each_stage_is_recorded_and_installation_orders_reverification(self):
        record_stage = capability(self, self.ledger, "record_stage")
        identifier, pub = self.opened()
        with self.assertRaises(faults.FaultRefused):
            capability(self, self.ledger, "record_stage")(identifier, stage="merged", ref="PR-1")
        self.publish(pub)
        record_stage(identifier, stage="accepted", ref="owner-1")
        record_stage(identifier, stage="assigned", ref="child-1")
        self.ledger.record_fix(identifier, ref="PR-1")
        self.ledger.record_reverification(identifier, method="suite", ref="pre-install",
                                          outcome="passed")
        record_stage(identifier, stage="merged", ref="PR-1")
        record_stage(identifier, stage="installed", ref="rev-9")
        self.assertEqual(sorted(capability(self, self.ledger, "progress")(identifier)),
                         ["accepted", "assigned", "installed", "merged"])
        with self.assertRaises(faults.FaultRefused) as refused:
            self.ledger.resolve(identifier)
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_VERIFICATION_STALE)
        self.ledger.record_reverification(identifier, method="suite", ref="post-install",
                                          outcome="passed")
        self.assertTrue(self.ledger.resolve(identifier)["resolved"])


class B11_UnsentWritesAndNotificationsAreVisible(ContractCase):
    """Post-merge blocker 11: nothing warned about unsent writes; no upward notification path."""

    def test_attention_counts_claimed_and_uncertain_writes(self):
        attention = capability(self, self.ledger, "attention")
        _, pub = self.opened()
        self.ledger.claim(pub, owner="writer-A")
        seen = attention()
        self.assertEqual(seen["unsent"]["claimed"], 1)
        self.assertIsNotNone(seen["warning"])

    def test_a_broken_fault_raises_a_blocking_notification_once(self):
        notifications = capability(self, self.ledger, "notifications")
        self.opened("k1")
        self.ledger.record(observation("k2"))
        kinds = [entry["kind"] for entry in notifications(limit=10)["notifications"]]
        self.assertEqual(kinds, ["blocking"])

    def test_a_paused_relationship_withholds_and_two_reservers_share_one_budget(self):
        self.register()
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET status = 'paused'")
        relationship = self.store.one("SELECT relationship_id FROM relationships")[0]
        identifier = self.ledger.record(observation(
            "k1", signature={"relationship": relationship, "turn": "t"}))["faultId"]
        reserve = capability(self, self.ledger, "reserve_notifications")
        self.assertEqual(reserve(owner="a", limit=5)["reserved"], [])
        listed = capability(self, self.ledger, "notifications")(limit=5)["notifications"]
        self.assertEqual(listed[0]["eligibility"]["eligible"], False)
        self.assertEqual(listed[0]["faultId"], identifier)

    def test_a_lapsed_reservation_is_uncertain_and_listed(self):
        self.opened()
        reserved = capability(self, self.ledger, "reserve_notifications")(owner="a", limit=5)["reserved"]
        self.assertEqual(len(reserved), 1)
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.assertEqual(capability(self, self.ledger, "reserve_notifications")(owner="b", limit=5)["reserved"], [])
        uncertain = capability(self, self.ledger, "notifications")(state="uncertain", limit=5)["notifications"]
        self.assertEqual([entry["deliveryKey"] for entry in uncertain],
                         [reserved[0]["deliveryKey"]])

    def test_a_caller_raised_decision_uses_the_same_path(self):
        raise_notification = capability(self, self.ledger, "raise_notification")
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        first = raise_notification(identifier, reason="awaiting_classification", ref="route-1")
        again = raise_notification(identifier, reason="awaiting_classification", ref="route-1")
        self.assertEqual(first["notificationId"], again["notificationId"])


class B12_SuppressionPolicyIsAdjustable(ContractCase):
    """Post-merge blocker 12: no path to read or adjust a suppression policy."""

    def test_a_degraded_threshold_can_be_lowered_prospectively(self):
        set_policy = capability(self, self.ledger, "set_policy")
        stall = {"recipient": "p", "attemptState": "held"}
        first = self.ledger.record(observation("s1", fault_class="delivery_stalled",
                                               severity=faults.DEGRADED, signature=stall))
        self.assertEqual(first["state"], faults.OBSERVED)
        set_policy(PRODUCT, "delivery_stalled", faults.DEGRADED, threshold=1,
                   reason="one stall is enough here")
        second = self.ledger.record(observation("s2", fault_class="delivery_stalled",
                                                severity=faults.DEGRADED, signature=stall))
        self.assertEqual(second["state"], faults.OPEN)
        self.assertIn("override", second["suppression"]["reason"])

    def test_a_broken_threshold_cannot_be_raised(self):
        set_policy = capability(self, self.ledger, "set_policy")
        with self.assertRaises(faults.FaultRefused) as refused:
            set_policy(PRODUCT, "report_omitted", faults.BROKEN, threshold=3, reason="quiet")
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_POLICY_FIXED)


class B13_TheWriterKeepsItsWrite(ContractCase):
    """Post-merge blocker 13: a failure dropped the writer and collapsed the retry history."""

    def test_another_writer_needs_a_recorded_takeover(self):
        _, pub = self.opened()
        claim = self.ledger.claim(pub, owner="writer-A")
        self.ledger.fail(pub, claim_token=claim["claimToken"], error="network down")
        self.clock.advance(faults.MAX_BACKOFF + 1)
        with self.assertRaises(faults.FaultRefused) as refused:
            self.ledger.claim(pub, owner="writer-B")
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_WRITER_CONFLICT)
        self.ledger.claim(pub, owner="writer-B", takeover=True)
        history = capability(self, self.ledger, "attempts")(pub, limit=5)
        self.assertEqual([(entry["owner"], entry["takeover"]) for entry in history],
                         [("writer-A", 0), ("writer-B", 1)])
        self.assertEqual(history[0]["error"], "network down")


class NB1_ClearsAreNotOccurrences(ContractCase):
    def test_one_failure_and_one_clear_count_one(self):
        identifier, pub = self.opened("k")
        self.publish(pub)
        self.ledger.record(observation("k", cleared=True))
        self.assertEqual(self.ledger.get(identifier)["occurrence_count"], 1)


class IssueSlot(ContractCase):
    """Invariant 1 and plan-audit rounds 5-8: one issue per fault on every path."""

    def test_a_clear_before_anything_landed_withdraws_and_cancels(self):
        identifier, pub = self.opened()
        answer = self.ledger.record(observation("k1", cleared=True))
        self.assertEqual(answer["state"], faults.WITHDRAWN)
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["state"], "cancelled")
        again = self.ledger.record(observation("k2"))
        self.assertEqual(again["publication"]["publicationId"], pub)
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["state"], faults.PENDING)

    def test_adopting_an_open_fault_cancels_its_create_and_comments(self):
        identifier, pub = self.opened()
        answer = capability(self, self.ledger, "adopt")(
            identifier, external_ref="REL-9", scope=SCOPE)
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["state"], "cancelled")
        self.assertEqual(self.ledger.get(identifier)["external_ref"], "REL-9")
        self.assertEqual(answer["publication"]["kind"], faults.APPEND_COMMENT)

    def test_adopting_after_a_failed_create_leaves_nothing_to_retry(self):
        identifier, pub = self.opened()
        for _ in range(faults.MAX_ATTEMPTS):
            claim = self.ledger.claim(pub, owner="writer-A")
            self.ledger.fail(pub, claim_token=claim["claimToken"], error="down")
            self.clock.advance(faults.MAX_BACKOFF + 1)
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["state"], faults.FAILED)
        capability(self, self.ledger, "adopt")(identifier, external_ref="REL-9", scope=SCOPE)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.retry(pub)

    def test_adopting_an_issued_create_is_refused(self):
        identifier, pub = self.opened()
        self.created(pub)
        with self.assertRaises(faults.FaultRefused) as refused:
            capability(self, self.ledger, "adopt")(identifier, external_ref="REL-9", scope=SCOPE)
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_ADOPT_CONFLICT)

    def test_adopting_at_first_record_never_queues_a_create(self):
        answer = self.ledger.record(observation("k"), adopt={"externalRef": "REL-9",
                                                             "scope": SCOPE})
        self.assertEqual(answer["publication"]["kind"], faults.APPEND_COMMENT)
        self.assertEqual(capability(self, self.ledger, "publications")(answer["faultId"], kind="open_record"), [])

    def test_adopting_an_unrecorded_fault_is_refused(self):
        with self.assertRaises(faults.FaultRefused) as refused:
            capability(self, self.ledger, "adopt")("0" * 32, external_ref="REL-9", scope=SCOPE)
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_UNKNOWN)

    def test_queue_never_creates_an_issue(self):
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        with self.assertRaises(faults.FaultRefused):
            capability(self, self.ledger, "queue")(identifier, kind="open_record", trigger="x")


class Kinds(ContractCase):
    """The extensible kind seam (CRW-206 items 3, 8, G1, G2, G5)."""

    def kind(self, name, *, pre_issue=None):
        capability(self, faults, "register_kind")(
            name, creates=True, requires_issue=False, target="team", evidence="block",
            confirm=lambda expected, observed: [], pre_issue=pre_issue)
        self.addCleanup(getattr(faults, "KINDS", {}).pop, name, None)

    def test_a_kind_queues_on_a_never_opened_fault_and_confirms_on_its_publication(self):
        self.kind("project_create_test")
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        queued = capability(self, self.ledger, "queue")(identifier, kind="project_create_test", trigger="need",
                                   payload={"name": "Ops"})
        pub = queued["publicationId"]
        claim, operation = self.created(pub)
        self.ledger.complete(pub, claim_token=claim["claimToken"],
                             readback=operation["block"], external_ref="PROJECT-7")
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["external_ref"], "PROJECT-7")
        self.assertIsNone(self.ledger.get(identifier)["external_ref"])

    def test_pre_issue_reads_the_same_transaction_and_cancels_without_an_attempt(self):
        seen = {}

        def check(context):
            seen["row"] = context["db"].execute(
                "SELECT COUNT(*) AS n FROM fault_publications WHERE publication_id = ?",
                (context["publication"]["publication_id"],)).fetchone()["n"]
            return {"cancel": "a suitable project was bound after the claim"}

        self.kind("project_create_cancel", pre_issue=check)
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        pub = capability(self, self.ledger, "queue")(identifier, kind="project_create_cancel",
                                trigger="need")["publicationId"]
        claim = self.ledger.claim(pub, owner="writer-A")
        with self.assertRaises(faults.FaultRefused):
            self.ledger.operation(pub, claim_token=claim["claimToken"])
        row = capability(self, self.ledger, "publication")(pub)
        self.assertEqual((seen["row"], row["state"], row["attempts"]), (1, "cancelled", 0))

    def test_a_writing_pre_issue_is_refused_and_its_write_discarded(self):
        def check(context):
            context["db"].execute("UPDATE fault_ledger SET detail = 'written'")
            return None

        self.kind("project_create_write", pre_issue=check)
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        pub = capability(self, self.ledger, "queue")(identifier, kind="project_create_write",
                                trigger="need")["publicationId"]
        claim = self.ledger.claim(pub, owner="writer-A")
        with self.assertRaises(faults.FaultRefused):
            self.ledger.operation(pub, claim_token=claim["claimToken"])
        self.assertNotEqual(self.ledger.get(identifier)["detail"], "written")
        self.assertEqual(capability(self, self.ledger, "publication")(pub)["state"], faults.CLAIMED)

    def test_an_unregistered_kind_is_never_offered(self):
        self.kind("project_create_gone")
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        pub = capability(self, self.ledger, "queue")(identifier, kind="project_create_gone",
                                trigger="need")["publicationId"]
        getattr(faults, "KINDS", {}).pop("project_create_gone")
        self.assertNotIn(pub, [entry["publication_id"] for entry in self.ledger.next(limit=10)])
        with self.assertRaises(faults.FaultRefused) as refused:
            self.ledger.claim(pub, owner="writer-A")
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_KIND_UNREGISTERED)


class Relinking(ContractCase):
    """Invariants 10 and 11: the issue ends on the current project."""

    def test_p0_p1_p2_ends_on_p2_whatever_order_completes(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P1")
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P2")
        live = [entry for entry in capability(self, self.ledger, "publications")(identifier, kind="update_record")
                if entry["state"] == faults.PENDING]
        self.assertEqual([entry["payload"]["value"] for entry in live], ["P2"])
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P1")
        live = [entry for entry in capability(self, self.ledger, "publications")(identifier, kind="update_record")
                if entry["state"] == faults.PENDING]
        self.assertEqual([entry["payload"]["value"] for entry in live], ["P1"])

    def test_an_observation_moving_the_scope_relinks_the_owned_issue(self):
        self.ledger.set_target(product=PRODUCT, project="OPS", team=TEAM, project_ref="proj-ops")
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.record(observation("k1", scope={"projectKey": "OPS"}))
        live = [entry["payload"]["value"] for entry in
                capability(self, self.ledger, "publications")(identifier, kind="update_record")
                if entry["state"] == faults.PENDING]
        self.assertEqual(live, ["proj-ops"])


class ClaimsAreChargedOnceEachAndStaleRelinksNeverIssue(ContractCase):
    """PR #142 review round one, each an instance of invariants 4, 11 and 12.

    A claim that never issued gives its unit and attempt back however it ends, lapsing
    included; every claim is charged as itself, so a retried write cannot match a unit an
    earlier claim spent; and a relink is issued only to the project the scope targets now.
    """

    def test_a_lapsed_claim_gives_back_its_unit(self):
        capability(self, self.ledger, "set_limit")(PRODUCT, "open_record", max_count=1, window=3600)
        _identifier, pub = self.opened()
        self.ledger.claim(pub, owner="writer-A")
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.ledger.expire_leases()
        try:
            self.ledger.claim(pub, owner="writer-A")
        except faults.FaultRefused as refusal:
            self.fail(f"a claim that issued nothing kept the product's only unit:"
                      f" {refusal.reason.value}")
        self.assertEqual(1, capability(self, self.ledger, "budget")(PRODUCT, "open_record")["used"])
        self.assertEqual(["lease_lapsed", None],
                         [entry["outcome"] for entry in capability(self, self.ledger, "attempts")(pub)])

    def test_a_retried_write_is_charged_again_and_keeps_its_history(self):
        capability(self, self.ledger, "set_limit")(PRODUCT, "open_record", max_count=faults.MAX_ATTEMPTS,
                              window=86400)
        _identifier, pub = self.opened()
        for _ in range(faults.MAX_ATTEMPTS):
            claim = self.ledger.claim(pub, owner="writer-A")
            self.ledger.fail(pub, claim_token=claim["claimToken"], error="refused before issue")
            self.clock.advance(faults.MAX_BACKOFF + 1)
        self.assertEqual(faults.FAILED, capability(self, self.ledger, "publication")(pub)["state"])
        self.ledger.retry(pub)
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.claim(pub, owner="writer-A")
        self.assertEqual(RefusalReason.FAULT_BUDGET_SPENT, refusal.exception.reason)
        capability(self, self.ledger, "set_limit")(PRODUCT, "open_record", max_count=faults.MAX_ATTEMPTS + 1,
                              window=86400)
        self.ledger.claim(pub, owner="writer-A")
        capability(self, self.ledger, "cancel")(pub, reason="the operator withdrew it")
        self.assertEqual(["failed_before_issue"] * faults.MAX_ATTEMPTS + ["cancelled"],
                         [entry["outcome"] for entry in capability(self, self.ledger, "attempts")(pub, limit=20)])

    def test_a_relink_is_never_issued_once_the_scope_has_no_project(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P1")
        relink = [entry["publication_id"]
                  for entry in capability(self, self.ledger, "publications")(identifier, kind="update_record")
                  if entry["state"] == faults.PENDING]
        self.assertEqual(1, len(relink))
        claim = self.ledger.claim(relink[0], owner="writer-A")
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref=None)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.operation(relink[0], claim_token=claim["claimToken"])
        self.assertEqual(faults.CANCELLED, capability(self, self.ledger, "publication")(relink[0])["state"])


class NoProjectMeansUnlinked(ContractCase):
    """Final review round one, blocker 1 (invariants 10 and 11, the project-linkage correction).

    With no project its product owns, an owned issue has nowhere it belongs: it is unlinked,
    awaiting a target, and never reported linked. Setting a project again relinks the same issue.
    """

    def link_state(self, identifier):
        return capability(self, self.ledger, "get")(identifier).get("linkState")

    def test_removing_the_project_unlinks_and_a_new_project_relinks_the_same_issue(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.assertEqual(faults.LINKED, self.link_state(identifier))
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref=None)
        self.assertEqual(faults.UNLINKED, self.link_state(identifier))
        self.assertEqual(1, capability(self, self.ledger, "attention")()["unlinked"])
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P2")
        self.assertEqual(["P2"], [
            entry["payload"]["value"]
            for entry in capability(self, self.ledger, "publications")(identifier,
                                                                     kind="update_record")
            if entry["state"] == faults.PENDING])

    def test_a_move_to_a_scope_without_a_project_unlinks(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.set_target(product=PRODUCT, project="OPS", team=TEAM)
        capability(self, self.ledger, "move")(identifier, scope={"projectKey": "OPS"})
        self.assertEqual(faults.UNLINKED, self.link_state(identifier))

    def test_a_create_confirmed_after_the_project_was_removed_is_not_linked(self):
        identifier, pub = self.opened()
        claim, operation = self.created(pub)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref=None)
        self.ledger.complete(pub, claim_token=claim["claimToken"],
                             readback="issue body\n\n" + operation["block"] + "\n",
                             external_ref="REL-1", project_ref=PROJECT)
        self.assertEqual(faults.UNLINKED, self.link_state(identifier))


class SelectionIsFairPastWhatItCannotTake(ContractCase):
    """Final review round one, blockers 2 and 3 (invariants 12 and 15): what cannot be taken now
    - a spent budget, a withheld notification - never hides what can be taken behind it."""

    def notice(self, relationship, product=PRODUCT, turn="t"):
        return self.ledger.record(observation(
            f"{relationship}:{turn}", product=product, severity=faults.NOTICE,
            signature={"relationship": relationship, "turn": turn}))["faultId"]

    def test_a_capped_product_does_not_hold_another_products_notification(self):
        raise_ = capability(self, self.ledger, "raise_notification")
        capability(self, self.ledger, "set_limit")(PRODUCT, "notification", max_count=1,
                                                   window=3600)
        for n in range(5):
            raise_(self.notice(f"r{n}"), reason="classification")
        self.assertEqual(1, len(self.ledger.reserve_notifications(owner="w", limit=1)["reserved"]))
        other = self.notice("r-ops", product="ops")
        raise_(other, reason="classification")
        reserved = self.ledger.reserve_notifications(owner="w", limit=1)["reserved"]
        self.assertEqual([other], [entry["faultId"] for entry in reserved])

    def test_withheld_notifications_at_the_head_do_not_hide_the_next(self):
        raise_ = capability(self, self.ledger, "raise_notification")
        self.register()
        relationship = self.store.one("SELECT relationship_id FROM relationships")[
            "relationship_id"]
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET status = 'paused'")
        for n in range(4):
            raise_(self.notice(relationship, turn=f"t{n}"), reason="classification")
        free = self.notice("r-free")
        raise_(free, reason="classification")
        self.ledger.reserve_notifications(owner="w", limit=1)
        reserved = self.ledger.reserve_notifications(owner="w", limit=1)["reserved"]
        self.assertEqual([free], [entry["faultId"] for entry in reserved])

    def test_many_spent_products_do_not_starve_an_open_one(self):
        count = 501
        with self.store.transaction() as db:
            for n in range(count + 1):
                product = f"p{n:03d}"
                db.execute("INSERT INTO fault_targets (scope_key, tracker_ref, recorded_at)"
                           " VALUES (?,?,?)", (product + ":CRW", TEAM, "t"))
                db.execute("INSERT INTO fault_target_projects (scope_key, product, project_ref,"
                           " recorded_at) VALUES (?,?,?,?)", (product + ":CRW", product, PROJECT, "t"))
                if n < count:
                    db.execute("INSERT INTO fault_limits (product, kind, max_count,"
                               " window_seconds, updated_at) VALUES (?,?,?,?,?)",
                               (product, "open_record", 1, 3600.0, "t"))
                    db.execute("INSERT INTO fault_budget_uses (product, kind, ref, used_at,"
                               " used_ts) VALUES (?,?,?,?,?)",
                               (product, "open_record", "spent", "t", self.clock.now()))
        for n in range(count + 1):
            self.ledger.record(observation(f"k{n}", product=f"p{n:03d}"))
        ready = self.ledger.next(limit=1)
        self.assertEqual([f"p{count:03d}"], [
            capability(self, self.ledger, "get")(entry["fault_id"])["product"]
            for entry in ready])


class NothingOutstandingHidesTheTruth(ContractCase):
    """Final review round two (invariants 11 and 15).

    A readback proves where an issue is only while no write to another project may still land;
    and every pending notification is examined in turn, whatever the caller's cadence.
    """

    def updates(self, identifier, state):
        return [entry for entry in capability(self, self.ledger, "publications")(
            identifier, kind="update_record") if entry["state"] == state]

    def test_an_outstanding_write_to_another_project_keeps_the_issue_unlinked(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P2")
        relink = self.updates(identifier, faults.PENDING)[0]["publication_id"]
        claim = self.ledger.claim(relink, owner="writer-A")
        self.ledger.operation(relink, claim_token=claim["claimToken"])
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref=PROJECT)
        get = capability(self, self.ledger, "get")
        self.assertEqual(faults.UNLINKED, get(identifier).get("linkState"),
                         "the write to P2 may still land, so the old readback proves nothing")
        self.assertEqual([], self.updates(identifier, faults.PENDING),
                         "no second write while the first may still land")
        self.ledger.fail(relink, claim_token=claim["claimToken"], error="response lost")
        self.assertEqual(1, capability(self, self.ledger, "attention")()["unlinked"])
        landed = {"issue": "REL-1", "projectId": "P2"}
        self.assertEqual("present", self.ledger.reconcile(relink, observed=landed)["outcome"])
        self.ledger.complete(relink, observed=landed)
        self.assertEqual(faults.UNLINKED, get(identifier).get("linkState"))
        self.assertEqual([PROJECT], [entry["payload"]["value"]
                                     for entry in self.updates(identifier, faults.PENDING)],
                         "the issue landed in P2 and is repaired on the same issue")

    def test_withheld_notifications_do_not_hide_a_later_one_at_any_cadence(self):
        raise_ = capability(self, self.ledger, "raise_notification")
        self.register()
        relationship = self.store.one("SELECT relationship_id FROM relationships")[
            "relationship_id"]
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET status = 'paused'")
        for n in range(4):
            raise_(self.ledger.record(observation(
                f"w{n}", severity=faults.NOTICE,
                signature={"relationship": relationship, "turn": f"t{n}"}))["faultId"],
                reason="classification")
        free = self.ledger.record(observation(
            "free", severity=faults.NOTICE,
            signature={"relationship": "r-free", "turn": "t"}))["faultId"]
        raise_(free, reason="classification")
        reserved = []
        for _ in range(2):
            reserved += self.ledger.reserve_notifications(owner="w", limit=1)["reserved"]
            self.clock.advance(61)
        self.assertEqual([free], [entry["faultId"] for entry in reserved])


class ExistenceQuestionsAreBounded(RelayTestCase):
    """Final review round one, blocker 4 (invariant 14): asking whether a fault's source still
    produces it judged every overtaken delivery in one call. Now one call judges a bounded number,
    keeps its verdicts, and a later call continues; an absence is only ever established over all
    of them."""

    def setUp(self):
        super().setUp()
        self.assertTrue(hasattr(faultsweep, "UNDETERMINED"),
                        "an existence question has no bound to answer against")
        self.register()
        self.relationship = self.store.one("SELECT relationship_id FROM relationships")[
            "relationship_id"]

    def delivery(self, event, generation):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO events (event_id, relationship_id, execution_generation,"
                " revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status,"
                " receipt, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                (event, self.relationship, generation, "h-" + event, "ready_for_review",
                 "child", "child-thread", "turn-" + event, "completed", "{}", "t", "t"))
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                " recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event, self.relationship, "completion", "p", "p", "withheld_pre_send", 6,
                 "attempt_cap", "t", "t"))

    def test_one_question_judges_a_bounded_number_and_the_next_continues(self):
        from unittest import mock

        from codex_session_relay import delivery

        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET execution_generation = 2")
        overtaken = faultsweep.PRESENT_CHECKS * 2 + 3
        for n in range(overtaken):
            self.delivery(f"old-{n:04d}", 1)
        calls = []
        original = delivery.supersession_reason

        def counted(db, event_id):
            calls.append(event_id)
            return original(db, event_id)

        signature = {"recipient": "p", "attemptState": None}
        answers = []
        with mock.patch.object(delivery, "supersession_reason", counted):
            for _ in range(3):
                before = len(calls)
                answers.append(faultsweep.still_present(self.store, "delivery_stalled",
                                                        signature))
                self.assertLessEqual(len(calls) - before, faultsweep.PRESENT_CHECKS)
        self.assertEqual([faultsweep.UNDETERMINED, faultsweep.UNDETERMINED, None], answers)
        self.assertEqual(overtaken, len(set(calls)))
        self.delivery("new", 2)
        self.assertTrue(faultsweep.still_present(self.store, "delivery_stalled", signature))
        self.assertIsNot(faultsweep.UNDETERMINED,
                         faultsweep.still_present(self.store, "delivery_stalled", signature))


class EveryCollectedIncidentStatesItsFacts(RelayTestCase):
    """Final review round one, blocker 5 (criterion 1): each automatically collected incident
    records what was expected, what happened, the impact, what the reading cannot see, and the
    installation it was seen under - the revision being a stated limit the relay cannot know."""

    def test_each_collected_observation_carries_its_facts(self):
        from codex_session_relay import __version__

        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                " recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                " updated_at) VALUES ('held', 'rel-1', 'completion', 'p1', 'p1',"
                " 'withheld_pre_send', 6, 'attempt_cap', 't', 't')")
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                " recipient_thread_id, state, attempt_count, created_at, updated_at)"
                " VALUES ('refused', 'rel-2', 'completion', 'p2', 'p2', 'withheld_pre_send', 0,"
                " 't', 't')")
            self.store.journal("delivery_withheld", "refused",
                               {"reason": "settings_incomplete", "detail": "no"}, at="t")
            db.execute(
                "INSERT INTO managed_start_requests (request_id, issue_key,"
                " request_fingerprint, fingerprint_version, workspace, marker_root,"
                " socket_identity, create_request_id, dispatch_request_id, state, revision,"
                " receipt_status, created_at, updated_at)"
                " VALUES ('req-1', 'CRW-9', 'fp', 'v1', '/w', '/m', 'sock', 'c', 'd',"
                " 'create_armed', 2, 'failed', 't', 't')")
        batch = faultsweep.sweep(self.store, readings=[{
            "schema": faultsweep.OBSERVATION_SCHEMA, "relationshipId": "rel-3",
            "selectors": {"turn": "t1"}, "reportingState": "unreported"}])
        active = [entry for entry in batch["observations"] if not entry["cleared"]]
        self.assertEqual({"delivery_stalled", "delivery_refused", "managed_start_failed",
                          "report_omitted"}, {entry["faultClass"] for entry in active})
        for entry in active:
            facts = [item["observed"] for item in entry["evidence"] if item["kind"] == "facts"]
            self.assertEqual(1, len(facts), entry["faultClass"])
            for field in ("expected", "actual", "impact"):
                self.assertTrue(facts[0].get(field), (entry["faultClass"], field))
            self.assertEqual(__version__, facts[0]["installation"]["version"])
            self.assertIn(faultsweep.INSTALLATION_LIMIT, facts[0]["limits"])


class BoundsHoldAndFactsNameTheirTurn(ContractCase):
    """Final review round three (criterion 1, invariants 14 and 15).

    A collected delivery incident names the generation and turn its event carries; re-pointing
    and the outstanding-write question read a bounded number of rows however many a scope or a
    fault holds; and two examinations of a notification never tie, so one examined at the same
    instant as those ahead of it is still reached.
    """

    def rows_read(self, action):
        """How many rows the store handed this process while action ran."""
        import sqlite3

        counted = [0]

        def counting(cursor, row):
            counted[0] += 1
            return sqlite3.Row(cursor, row)

        self.store.db.row_factory = counting
        try:
            action()
        finally:
            self.store.db.row_factory = sqlite3.Row
        return counted[0]

    def test_a_delivery_incident_names_its_generation_and_turn(self):
        self.register()
        current = self.store.one("SELECT relationship_id, execution_generation"
                                 " FROM relationships")
        relationship, generation = current["relationship_id"], current["execution_generation"]
        with self.store.transaction() as db:
            for event, state, attempts, hold in (
                    ("held", "withheld_pre_send", 6, "attempt_cap"),
                    ("retried", "queued", 1, None),
                    ("refused", "withheld_pre_send", 0, None)):
                # An execution-only outcome of the current generation: never overtaken.
                db.execute(
                    "INSERT INTO events (event_id, relationship_id, execution_generation,"
                    " revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status,"
                    " receipt, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                    (event, relationship, generation, "h-" + event, "failed", "child",
                     "child-thread", "turn-" + event, "failed", "{}", "t", "t"))
                db.execute(
                    "INSERT INTO deliveries (event_id, relationship_id, kind,"
                    " recipient_task_id, recipient_thread_id, state, attempt_count,"
                    " hold_reason, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (event, relationship, "completion", "p", "p", state, attempts, hold,
                     "t", "t"))
            db.execute(
                "INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state,"
                " state, sent_at, observed_at) VALUES (?,?,?,?,?,?,?,?)",
                ("req-retried", "retried", 1, "completion", "settled", "withheld_pre_send",
                 self.clock.now(), "t"))
            self.store.journal("delivery_withheld", "refused",
                               {"reason": "settings_incomplete", "detail": "no"}, at="t")
        batch = faultsweep.sweep(self.store)
        named = {}
        for entry in batch["observations"]:
            for item in entry["evidence"]:
                subject = item["observed"] if item["kind"] == "facts" else {}
                if subject.get("event") in ("held", "retried", "refused"):
                    named[(entry["faultClass"], subject["event"])] = (
                        subject.get("generation"), subject.get("turn"))
        self.assertEqual({
            ("delivery_stalled", "held"): (generation, "turn-held"),
            ("delivery_stalled", "retried"): (generation, "turn-retried"),
            ("delivery_refused", "refused"): (generation, "turn-refused"),
        }, named)

    def test_a_target_change_re_points_a_bounded_batch_and_relink_finishes_it(self):
        extra = 7
        count = faults.RELINK_PER_CALL + extra
        for n in range(count):
            self.ledger.record(observation(f"k{n}", signature={"relationship": f"r{n}",
                                                                "turn": "t"}))
        answer = self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM,
                                        project_ref="P2")
        self.assertEqual((faults.RELINK_PER_CALL, extra),
                         (answer["backfilled"], answer.get("backfillPending")))
        rest = self.ledger.relink()
        self.assertEqual((extra, 0), (rest.get("backfilled"), rest.get("backfillPending")))
        where = self.store.all(
            "SELECT pp.project_ref AS project, COUNT(*) AS n FROM fault_publications p"
            "  JOIN fault_publication_payloads pp ON pp.publication_id = p.publication_id"
            " WHERE p.kind = ? AND p.state = ? GROUP BY pp.project_ref",
            (faults.OPEN_RECORD, faults.PENDING))
        self.assertEqual([("P2", count)], [(row["project"], row["n"]) for row in where])

    def test_the_outstanding_write_question_reads_one_row_however_many_were_issued(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P2")
        stamp = self.clock.iso()
        issued = 1200
        with self.store.transaction() as db:
            for n in range(issued):
                publication = f"seeded-{n:04d}"
                db.execute(
                    "INSERT INTO fault_publications (publication_id, fault_id, kind,"
                    " trigger_key, tracker_ref, summary, identity_digest, state, created_at,"
                    " updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (publication, identifier, faults.UPDATE_RECORD,
                     f"update:set_project:P-{n}:r{n}", TEAM, "relink", "digest",
                     faults.ISSUED, stamp, stamp))
                db.execute(
                    "INSERT INTO fault_publication_payloads (publication_id, project_ref,"
                    " payload, updated_at) VALUES (?,?,?,?)",
                    (publication, None, json.dumps({"op": "set_project", "value": f"P-{n}"}),
                     stamp))
        read = self.rows_read(lambda: self.ledger.set_target(
            product=PRODUCT, project="CRW", team=TEAM, project_ref=PROJECT))
        self.assertLess(read, 100, f"{read} rows read to ask whether one write is outstanding")
        self.assertEqual(faults.UNLINKED,
                         capability(self, self.ledger, "get")(identifier).get("linkState"),
                         "an issued write to another project may still land")

    def test_notifications_examined_at_one_instant_are_still_reached_in_turn(self):
        raise_ = capability(self, self.ledger, "raise_notification")
        capability(self, self.ledger, "set_limit")(PRODUCT, "notification", max_count=50,
                                                   window=3600)
        self.register()
        registered = self.store.one("SELECT relationship_id, issue_key FROM relationships")
        relationship = registered["relationship_id"]
        # Its parent's contact is unmeasured, so every notification it governs is withheld.
        for n in range(4):
            raise_(self.ledger.record(observation(
                f"w{n}", severity=faults.NOTICE,
                signature={"relationship": relationship, "turn": f"t{n}"}))["faultId"],
                reason="classification")
        # Governed through its issue until that issue is no longer the relationship's.
        free = self.ledger.record(observation(
            "free", severity=faults.NOTICE, signature={"relationship": "r-free", "turn": "t"},
            scope={"projectKey": "CRW", "issueKey": registered["issue_key"]}))["faultId"]
        raise_(free, reason="classification")
        first = self.ledger.reserve_notifications(owner="w", limit=2)
        self.assertEqual(([], 5), (first["reserved"], first["withheld"]),
                         "all five examined at one instant")
        with self.store.transaction() as db:
            db.execute("UPDATE relationships SET issue_key = 'CRW-OTHER'")
        reserved = []
        for _ in range(2):
            # The clock does not move: every earlier examination happened at this instant.
            reserved += self.ledger.reserve_notifications(owner="w", limit=1)["reserved"]
        self.assertEqual([free], [entry["faultId"] for entry in reserved])

    def test_lapsed_leases_are_released_in_bounded_batches(self):
        capability(self, self.ledger, "set_limit")(PRODUCT, "open_record", max_count=500,
                                                   window=3600)
        extra = 3
        for n in range(faults.RELINK_PER_CALL + extra):
            _, pub = self.opened(f"k{n}", signature={"relationship": f"r{n}", "turn": "t"})
            self.ledger.claim(pub, owner="writer-A")
        self.clock.advance(faults.LEASE_SECONDS + 1)
        first = self.ledger.expire_leases()
        self.assertEqual((faults.RELINK_PER_CALL, True), (first["released"], first.get("more")))
        rest = self.ledger.expire_leases()
        self.assertEqual((extra, False), (rest["released"], rest.get("more")))

    def test_a_target_returning_to_where_the_issue_is_cancels_the_unsent_relink(self):
        identifier, pub = self.opened()
        self.publish(pub)
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref="P2")
        self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM, project_ref=PROJECT)
        unsent = [entry["payload"]["value"] for entry in capability(
            self, self.ledger, "publications")(identifier, kind="update_record")
            if entry["state"] in (faults.PENDING, faults.FAILED, faults.CLAIMED)]
        self.assertEqual([], unsent, "the issue already sits in the project the scope targets")
        self.assertEqual(faults.LINKED,
                         capability(self, self.ledger, "get")(identifier).get("linkState"))


class ASweepJudgesOnlyWhatItsRowsCanShow(RelayTestCase):
    """PR #142 hosted review (invariants 9 and 16): this store's rows carry no workspace, so a
    sweep neither clears nor holds open a fault recorded under another workspace; it judges the
    faults its own observations resolve to."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)

    def stalled(self, workspace, key):
        scope = {"projectKey": "CRW", **({"workspace": workspace} if workspace else {})}
        return self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.BROKEN,
            signature={"recipient": "p", "attemptState": None}, occurrence_key=key,
            scope=scope, detail="held", evidence=[{"kind": "row", "ref": "deliveries"}],
        ))["faultId"]

    def swept(self):
        faultsweep.record_all(self.ledger, faultsweep.sweep(self.store), store=self.store)

    def test_another_workspaces_fault_is_left_to_that_workspace(self):
        own = self.stalled(None, "own:1")
        other = self.stalled("ws-B", "other:1")
        self.assertNotEqual(own, other)
        self.swept()
        self.assertIsNotNone(self.ledger.get(own)["cleared_at"],
                             "this store's own fault clears once its rows are gone")
        self.assertIsNone(self.ledger.get(other)["cleared_at"],
                          "no row of this store speaks for another workspace's relay")

    def test_a_fault_moved_out_of_this_workspace_is_still_judged_under_its_id(self):
        own = self.stalled("unassigned", "own:1")
        capability(self, self.ledger, "move")(own, scope={"projectKey": "CRW",
                                                          "workspace": "ws-B"})
        with self.store.transaction() as db:
            self.assertEqual("ws-B", json.loads(db.execute(
                "SELECT scope FROM fault_ledger WHERE fault_id = ?", (own,)).fetchone()[
                "scope"])["workspace"])
        faultsweep.record_all(self.ledger, faultsweep.sweep(
            self.store, scope={"workspace": "unassigned"}), store=self.store)
        self.assertIsNotNone(self.ledger.get(own)["cleared_at"])


class RepointingNeverDependsOnTheCallingProcess(ContractCase):
    """Final review round four (invariants 10 and 14).

    A write is re-pointed from the target requirement recorded when it was queued, so a process
    that never loaded an extension kind's module still moves that kind's writes; and a repeated
    call with an unchanged target still reports what an earlier change left.
    """

    def external(self, name):
        capability(self, faults, "register_kind")(
            name, creates=False, requires_issue=False, target="team+project",
            evidence="block", confirm=lambda expected, observed: [])
        self.addCleanup(faults.KINDS.pop, name, None)

    def queued(self, name):
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        publication = capability(self, self.ledger, "queue")(
            identifier, kind=name, trigger="one")["publicationId"]
        return identifier, publication

    def project_of(self, publication):
        return capability(self, self.ledger, "publication")(publication)["target"]["projectRef"]

    def test_a_target_change_re_points_a_kind_this_process_never_loaded(self):
        self.external("external_kind_set")
        _, publication = self.queued("external_kind_set")
        self.assertEqual(PROJECT, self.project_of(publication))
        faults.KINDS.pop("external_kind_set")  # a later process without the kind's module
        answer = self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM,
                                        project_ref="P2")
        self.assertEqual((1, 0), (answer["backfilled"], answer["backfillPending"]))
        self.assertEqual("P2", self.project_of(publication))

    def test_a_move_re_points_a_kind_this_process_never_loaded(self):
        self.external("external_kind_move")
        identifier, publication = self.queued("external_kind_move")
        self.ledger.set_target(product=PRODUCT, project="OPS", team=TEAM, project_ref="proj-ops")
        faults.KINDS.pop("external_kind_move")
        moved = capability(self, self.ledger, "move")(identifier, scope={"projectKey": "OPS"})
        self.assertEqual((1, 0), (moved["repointed"], moved.get("repointPending")))
        self.assertEqual("proj-ops", self.project_of(publication))

    def test_an_unchanged_repeat_still_reports_what_an_earlier_change_left(self):
        extra = 1
        for n in range(faults.RELINK_PER_CALL + extra):
            self.ledger.record(observation(f"k{n}", signature={"relationship": f"r{n}",
                                                                "turn": "t"}))
        first = self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM,
                                       project_ref="P2")
        self.assertEqual((faults.RELINK_PER_CALL, extra),
                         (first["backfilled"], first["backfillPending"]))
        again = self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM,
                                       project_ref="P2")
        self.assertEqual((False, 0, extra),
                         (again["changed"], again["backfilled"], again["backfillPending"]),
                         "a retried call must not read the remaining work as done")
        self.assertEqual(extra, self.ledger.relink()["backfilled"])
        self.assertEqual(0, self.ledger.set_target(product=PRODUCT, project="CRW", team=TEAM,
                                                   project_ref="P2")["backfillPending"])

    def test_a_stored_limit_is_listed_where_its_kind_was_never_loaded(self):
        self.external("external_kind_limit")
        capability(self, self.ledger, "set_limit")(PRODUCT, "external_kind_limit", max_count=3,
                                                   window=3600)
        faults.KINDS.pop("external_kind_limit")
        listed = {entry["kind"]: entry["limit"]
                  for entry in capability(self, self.ledger, "limits")(PRODUCT)}
        self.assertEqual(3, listed.get("external_kind_limit"))

    def test_a_repeated_move_still_reports_what_is_left(self):
        """Final review round five (invariant 14): a retried move is not a finished one."""
        self.external("external_kind_many")
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        queue = capability(self, self.ledger, "queue")
        extra = 1
        for n in range(faults.RELINK_PER_CALL + extra):
            queue(identifier, kind="external_kind_many", trigger=f"t{n}")
        self.ledger.set_target(product=PRODUCT, project="OPS", team=TEAM, project_ref="proj-ops")
        move = capability(self, self.ledger, "move")
        first = move(identifier, scope={"projectKey": "OPS"})
        self.assertEqual((faults.RELINK_PER_CALL, extra),
                         (first["repointed"], first.get("repointPending")))
        again = move(identifier, scope={"projectKey": "OPS"})
        self.assertEqual((0, extra), (again["repointed"], again.get("repointPending")),
                         "a retried move must not read the remaining writes as re-pointed")
        self.ledger.relink()
        done = move(identifier, scope={"projectKey": "OPS"})
        self.assertEqual((0, 0), (done["repointed"], done.get("repointPending")))

    def test_a_lapsed_issued_write_is_warned_about_before_any_expiry(self):
        """Final review round five (criterion 7): an issued write nobody holds any more is
        visible on the next status and tick, whether or not leases were expired."""
        _, pub = self.opened()
        self.created(pub)
        attention = capability(self, self.ledger, "attention")
        self.assertIsNone(attention()["warning"], "a write in flight warns nobody")
        self.clock.advance(faults.LEASE_SECONDS + 1)
        lapsed = attention()
        self.assertIsNotNone(lapsed["warning"])
        self.assertEqual(1, lapsed["unsent"].get("issuedLapsed"))

    def test_a_lapsed_notification_reservation_is_warned_about_before_any_lapse(self):
        """The same class for notifications: a reservation nobody holds is uncertain."""
        identifier = self.ledger.record(observation("k", severity=faults.NOTICE))["faultId"]
        capability(self, self.ledger, "raise_notification")(identifier, reason="classification")
        self.assertEqual(1, len(self.ledger.reserve_notifications(owner="w", limit=1)["reserved"]))
        attention = capability(self, self.ledger, "attention")
        self.assertIsNone(attention()["warning"], "a live reservation warns nobody")
        self.clock.advance(faults.LEASE_SECONDS + 1)
        lapsed = attention()
        self.assertIsNotNone(lapsed["warning"])
        self.assertEqual(1, lapsed["notifications"].get("reservedLapsed"))


class LegacyScopeKeys(ContractCase):
    """Invariants 7 and 8 against a store written before products were restricted."""

    def seed_legacy(self):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity,"
                " signature, scope, scope_key, state, cycle, occurrence_count, reopen_count,"
                " first_seen_at, last_seen_at, updated_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,1,1,0,?,?,?)",
                ("legacyfault", "a:b", "report_omitted", "reporting", "broken",
                 '{"relationship":"x"}', "{}", "a:b", "open", "t", "t", "t"))
            db.execute("INSERT INTO fault_targets (scope_key, tracker_ref, recorded_at)"
                       " VALUES ('a:b', 'legacy-team', 't')")

    def test_a_key_another_product_carries_is_refused_everywhere(self):
        self.seed_legacy()
        with self.assertRaises(faults.FaultRefused) as refused:
            self.ledger.record(observation("k", product="a", scope={"projectKey": "b"}))
        self.assertEqual(refused.exception.reason, RefusalReason.FAULT_SCOPE_CONFLICT)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.set_target(product="a", project="b", team=TEAM, project_ref="p")

    def test_an_ownerless_legacy_target_never_issues(self):
        with self.store.transaction() as db:
            db.execute("INSERT INTO fault_targets (scope_key, tracker_ref, recorded_at)"
                       " VALUES ('crw:LEGACY', 'legacy-team', 't')")
        answer = self.ledger.record(observation("k", scope={"projectKey": "LEGACY"}))
        self.assertIsNotNone(answer["publication"])
        self.assertEqual(self.ledger.next(limit=5), [])
