"""The fault ledger: one breakage, one record, one issue, closed only by reverification.

Every test here moves an injected clock and touches no network. The publication boundary is
exercised by handing the module the text a connector would have read back, which is the whole
point of that boundary existing.
"""

import ast
import json
import pathlib
import unittest

from codex_session_relay import faults, faultsweep
from codex_session_relay.clock import FakeClock
from codex_session_relay.store import Store

from .support import RelayTestCase
from .test_delivery import DeliveryTestCase

SOURCE = pathlib.Path(faults.__file__).parent
PRODUCT = "crw"
SCOPE = {"projectKey": "CRW", "issueKey": "CRW-205"}
TRACKER = "team-relay"


def omission(key, *, turn="turn-7", severity=faults.BROKEN, cleared=False, evidence=None):
    return faults.observation(
        product=PRODUCT, fault_class="report_omitted", severity=severity,
        signature={"relationship": "rel-1", "turn": turn}, occurrence_key=key, scope=SCOPE,
        detail="an admitted turn settled without a report", cleared=cleared,
        evidence=evidence if evidence is not None else
        [{"kind": "row", "ref": "events", "observed": {"rows": 0}}],
    )


def stall(key, *, severity=faults.DEGRADED):
    return faults.observation(
        product=PRODUCT, fault_class="delivery_stalled", severity=severity,
        signature={"recipient": "01parent-task", "cause": "channel_closed",
                   "attemptState": "withheld_pre_send"},
        occurrence_key=key, scope=SCOPE, detail="held",
        evidence=[{"kind": "row", "ref": "deliveries:e1", "observed": {"state": "queued"}}],
    )


class LedgerCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")

    def publish(self, identifier, publication, *, external_ref="REL-77"):
        """Carry one publication all the way to confirmed, as a connector holder would."""
        claim = self.ledger.claim(publication, owner="operator")
        operation = self.ledger.operation(publication, claim_token=claim["claimToken"])
        return self.ledger.complete(publication, project_ref="proj-CRW", claim_token=claim["claimToken"],
            readback="a document\n\n" + operation["block"] + "\n\nmore text",
            external_ref=external_ref,
        )


class Identity(LedgerCase):
    def test_the_same_fault_observed_again_converges_on_one_record(self):
        first = self.ledger.record(omission("observation:rel-1:turn-7"))
        again = self.ledger.record(omission("observation:rel-1:turn-7"))
        self.assertTrue(first["recorded"])
        self.assertFalse(again["recorded"])
        self.assertIsNone(again["publication"])
        self.assertEqual(
            1, self.store.one("SELECT COUNT(*) AS n FROM fault_ledger")["n"])
        self.assertEqual(
            1, self.store.one("SELECT COUNT(*) AS n FROM fault_occurrences")["n"])

    def test_identity_survives_a_restart_and_a_second_process(self):
        identifier = self.ledger.record(omission("a"))["faultId"]
        reopened = Store(str(self.store.path))
        self.addCleanup(reopened.close)
        second = faults.FaultLedger(reopened, FakeClock(start=self.clock.now() + 60))
        again = second.record(omission("a"))
        self.assertFalse(again["recorded"])
        self.assertEqual(identifier, again["faultId"])

    def test_two_deliveries_stranded_by_one_recipient_are_one_fault(self):
        first = self.ledger.record(stall("delivery:del-a"))
        second = self.ledger.record(stall("delivery:del-b"))
        self.assertEqual(first["faultId"], second["faultId"])
        self.assertEqual(2, second["occurrenceCount"])

    def test_signature_order_does_not_change_identity(self):
        self.assertEqual(
            faults.fault_id(PRODUCT, "report_omitted", {"a": 1, "b": 2}),
            faults.fault_id(PRODUCT, "report_omitted", {"b": 2, "a": 1}),
        )

    def test_an_unregistered_class_is_refused_because_nothing_would_clear_it(self):
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.record(faults.observation(
                product=PRODUCT, fault_class="invented", severity=faults.BROKEN,
                signature={"a": 1}, occurrence_key="k"))
        self.assertEqual("fault_class_unregistered", refusal.exception.reason.value)

    def test_a_class_without_a_clear_source_cannot_be_registered(self):
        with self.assertRaises(ValueError):
            faults.register_class("no_clear", component="delivery", clears="")


class Evidence(LedgerCase):
    def test_an_occurrence_keeps_what_was_observed_rather_than_a_pointer(self):
        self.ledger.record(omission("a", evidence=[
            {"kind": "row", "ref": "sync_outbox:s1", "observed": {"state": "failed"}}]))
        stored = self.ledger.occurrences(
            faults.fault_id(PRODUCT, "report_omitted",
                            {"relationship": "rel-1", "turn": "turn-7"}))[0]
        self.assertEqual("failed", stored["evidence"][0]["observed"]["state"])
        self.assertEqual(faults.evidence_digest(stored["evidence"]),
                         stored["evidence_digest"])

    def test_evidence_is_bounded_and_says_that_it_was(self):
        many = [{"kind": "row", "ref": f"r{index}", "observed": {"index": index}}
                for index in range(faults.MAX_EVIDENCE + 5)]
        self.ledger.record(omission("a", evidence=many))
        stored = self.ledger.occurrences(
            faults.fault_id(PRODUCT, "report_omitted",
                            {"relationship": "rel-1", "turn": "turn-7"}))[0]
        self.assertEqual(faults.MAX_EVIDENCE, len(stored["evidence"]))
        self.assertTrue(stored["truncated"])


class Suppression(LedgerCase):
    def test_broken_files_on_the_first_observation(self):
        answer = self.ledger.record(omission("a"))
        self.assertEqual(faults.OPEN, answer["state"])
        self.assertTrue(answer["publication"]["queued"])

    def test_degraded_waits_for_the_threshold_and_queues_once(self):
        first = self.ledger.record(stall("delivery:1"))
        second = self.ledger.record(stall("delivery:2"))
        self.assertEqual(faults.OBSERVED, first["state"])
        self.assertIsNone(first["publication"])
        self.assertEqual(faults.OBSERVED, second["state"])
        third = self.ledger.record(stall("delivery:3"))
        self.assertEqual(faults.OPEN, third["state"])
        self.assertTrue(third["publication"]["queued"])
        fourth = self.ledger.record(stall("delivery:4"))
        self.assertIsNone(fourth["publication"])

    def test_occurrences_outside_the_window_do_not_reach_the_threshold(self):
        self.ledger.record(stall("delivery:1"))
        self.clock.advance(faults.DEFAULT_WINDOW + 1)
        self.ledger.record(stall("delivery:2"))
        answer = self.ledger.record(stall("delivery:3"))
        self.assertEqual(faults.OBSERVED, answer["state"])
        self.assertEqual(2, answer["suppression"]["counted"])

    def test_a_notice_is_recorded_and_never_filed(self):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="observation_unmeasured", severity=faults.NOTICE,
            signature={"relationship": "rel-1"}, occurrence_key="u1", scope=SCOPE))
        self.assertEqual(faults.OBSERVED, answer["state"])
        self.assertIsNone(answer["publication"])
        self.assertFalse(answer["suppression"]["publish"])
        self.assertEqual([], self.ledger.next())


class ThirdReviewFindings(RelayTestCase):
    """Round three, on the code. Each finding keeps its own test."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        self.identifier = self.ledger.record(omission("a"))["faultId"]
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.complete(job, project_ref="proj-CRW", claim_token=claim["claimToken"],
                             readback=operation["block"], external_ref="REL-77")

    def test_the_same_check_run_again_after_a_second_fix_is_a_second_verification(self):
        """Otherwise the second run takes the first run's identity and resolve refuses forever."""
        self.ledger.record_fix(self.identifier, ref="PR #1")
        first = self.ledger.record_reverification(
            self.identifier, method="suite", ref="pytest -q", outcome=faults.PASSED)
        self.ledger.record_fix(self.identifier, ref="PR #2")
        second = self.ledger.record_reverification(
            self.identifier, method="suite", ref="pytest -q", outcome=faults.PASSED)
        self.assertTrue(second["recorded"])
        self.assertNotEqual(first["remediationId"], second["remediationId"])
        self.assertTrue(self.ledger.resolve(self.identifier)["resolved"])

    def test_recording_one_check_twice_with_no_fix_between_is_still_one_record(self):
        self.ledger.record_fix(self.identifier, ref="PR #1")
        first = self.ledger.record_reverification(
            self.identifier, method="suite", ref="pytest -q", outcome=faults.PASSED)
        again = self.ledger.record_reverification(
            self.identifier, method="suite", ref="pytest -q", outcome=faults.PASSED)
        self.assertFalse(again["recorded"])
        self.assertEqual(first["remediationId"], again["remediationId"])

    def test_a_locally_fixed_fault_that_never_reached_linear_is_withdrawn_when_it_clears(self):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="observation_unmeasured", severity=faults.NOTICE,
            signature={"relationship": "rel-9"}, occurrence_key="u1", scope=SCOPE))
        self.ledger.record_fix(answer["faultId"], ref="PR #1")
        self.assertEqual(faults.FIX_PENDING, self.ledger.get(answer["faultId"])["state"])
        cleared = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="observation_unmeasured", severity=faults.NOTICE,
            signature={"relationship": "rel-9"}, occurrence_key="u1:cleared", scope=SCOPE,
            cleared=True))
        self.assertEqual(faults.WITHDRAWN, cleared["state"])

    def test_a_published_fault_is_not_closed_by_its_cause_going_quiet(self):
        cleared = self.ledger.record(omission("a:cleared", cleared=True))
        self.assertEqual(faults.OPEN, cleared["state"])


class TheSweepRotatesRatherThanReReadingOnePage(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.register()
        self.relationship = self.store.one(
            "SELECT relationship_id FROM relationships")["relationship_id"]

    def stall(self, event_id, recipient):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event_id, self.relationship, "completion_event", recipient, recipient,
                 "queued", 1, "channel_closed", self.clock.iso(), self.clock.iso()))

    def test_a_fault_past_the_first_page_is_reached_by_a_later_sweep(self):
        """A fixed prefix re-read every tick starves everything behind it forever."""
        total = faultsweep.SWEEP_LIMIT + 3
        for index in range(total):
            self.stall(f"event-{index:03d}", f"task-{index:03d}")
        seen = set()
        for _round in range(4):
            batch = faultsweep.sweep(self.store)
            faultsweep.record_all(self.ledger, batch, store=self.store)
            seen.update(entry["signature"]["recipient"] for entry in batch["observations"]
                        if entry["faultClass"] == "delivery_stalled")
        self.assertEqual(total, len(seen))
        self.assertEqual(total, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_ledger WHERE fault_class = ?",
            ("delivery_stalled",))["n"])

    def test_a_short_page_wraps_so_the_next_sweep_starts_again(self):
        self.stall("event-001", "task-001")
        first = faultsweep.sweep(self.store)
        self.assertIsNone(first["cursors"]["delivery_stalled"])
        self.assertIn("delivery_stalled", first["completeSources"])
        second = faultsweep.sweep(self.store)
        self.assertEqual(1, len([entry for entry in second["observations"]
                                 if entry["faultClass"] == "delivery_stalled"]))

    def test_clearing_before_the_threshold_withdraws_without_touching_linear(self):
        self.ledger.record(stall("delivery:1"))
        answer = self.ledger.record(stall("cleared:after:1"))
        self.assertEqual(faults.OBSERVED, answer["state"])
        cleared = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.NOTICE,
            signature={"recipient": "01parent-task", "cause": "channel_closed",
                       "attemptState": "withheld_pre_send"},
            occurrence_key="cleared:after:x", scope=SCOPE, cleared=True))
        self.assertEqual(faults.WITHDRAWN, cleared["state"])
        self.assertEqual([], self.ledger.next())

    def test_pruning_evidence_does_not_change_what_the_next_observation_decides(self):
        identifier = self.ledger.record(stall("delivery:1"))["faultId"]
        self.ledger.record(stall("delivery:2"))
        self.ledger.prune(identifier, keep=1)
        answer = self.ledger.record(stall("delivery:3"))
        self.assertEqual(3, answer["suppression"]["counted"])
        self.assertEqual(faults.OPEN, answer["state"])


class FourthReviewFindings(RelayTestCase):
    """Round four. Two reds, and the quiet ones that would have been worse."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.register()
        self.relationship = self.store.one(
            "SELECT relationship_id FROM relationships")["relationship_id"]

    def stall(self, event_id="event-a", recipient="01parent-task"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event_id, self.relationship, "completion_event", recipient, recipient,
                 "queued", 1, "channel_closed", self.clock.iso(), self.clock.iso()))

    def drain(self):
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET hold_reason = NULL, state = 'dispatched'")

    def test_a_fault_whose_scope_moved_is_repointed_at_the_new_project(self):
        self.ledger.set_target(product="crw", project="NEW", team="team-new", project_ref="proj-NEW")
        first = self.ledger.record(omission("a"))
        self.assertTrue(first["publication"]["awaitingTarget"])
        moved = dict(omission("b"), scope={"projectKey": "NEW", "issueKey": "NEW-1"})
        self.ledger.record(moved)
        row = self.store.one(
            "SELECT tracker_ref FROM fault_publications WHERE publication_id = ?",
            (first["publication"]["publicationId"],))
        self.assertEqual("team-new", row["tracker_ref"])
        self.assertIn(first["faultId"], [job["fault_id"] for job in self.ledger.next()])

    def test_a_fix_recorded_before_the_threshold_does_not_suppress_the_record_forever(self):
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        first = self.ledger.record(stall("delivery:1"))
        self.assertEqual(faults.OBSERVED, first["state"])
        self.ledger.record_fix(first["faultId"], ref="PR #1")
        self.ledger.record(stall("delivery:2"))
        third = self.ledger.record(stall("delivery:3"))
        self.assertEqual(faults.OPEN, third["state"])
        self.assertTrue(third["publication"]["queued"])
        self.assertEqual(faults.OPEN_RECORD, third["publication"]["kind"])
        self.assertEqual([third["publication"]["publicationId"]],
                         [job["publication_id"] for job in self.ledger.next()])

    def test_a_source_larger_than_one_page_still_clears_what_recovered(self):
        """Differencing against a page stopped clearing exactly when a store got busy."""
        for index in range(faultsweep.SWEEP_LIMIT + 5):
            self.stall(f"event-{index:03d}", f"task-{index:03d}")
        for _round in range(3):
            batch = faultsweep.sweep(self.store)
            faultsweep.record_all(self.ledger, batch, store=self.store)
        self.drain()
        cleared = 0
        for _round in range(4):
            batch = faultsweep.sweep(self.store)
            faultsweep.record_all(self.ledger, batch, store=self.store)
            cleared += len(batch["clears"])
        self.assertGreater(cleared, 0)
        self.assertEqual(0, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_ledger"
            " WHERE fault_class = ? AND cleared_at IS NULL",
            ("delivery_stalled",))["n"])

    def test_evidence_this_cannot_record_is_reported_as_truncation(self):
        self.ledger.record(omission("a", evidence=[
            {"kind": "row", "ref": "events", "observed": {}}, "not an object"]))
        stored = self.ledger.occurrences(
            faults.fault_id(PRODUCT, "report_omitted",
                            {"relationship": "rel-1", "turn": "turn-7"}))[0]
        self.assertEqual(1, len(stored["evidence"]))
        self.assertTrue(stored["truncated"])

    def test_generation_ten_is_not_skipped_by_a_cursor_that_stopped_at_nine(self):
        page = faultsweep._page([], [{"relationship_id": "rel-x",
                                      "execution_generation": 9}], "anchor", None, 1)
        self.assertEqual("rel-x:" + "9".rjust(20, "0"), page["cursor"]["at"])
        self.assertLess(page["cursor"]["at"], "rel-x:" + "10".rjust(20, "0"))

    def test_a_cleared_flag_that_is_not_a_boolean_is_refused(self):
        bad = dict(omission("a"), cleared="false")
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.record(bad)
        self.assertEqual("fault_observation_malformed", refusal.exception.reason.value)

    def test_a_healthy_reading_records_no_fault_at_all(self):
        answer = self.ledger.record(omission("a:reported", cleared=True))
        self.assertFalse(answer["recorded"])
        self.assertEqual(0, self.store.one("SELECT COUNT(*) AS n FROM fault_ledger")["n"])

    def test_registering_a_class_again_with_different_terms_is_refused(self):
        faults.register_class("report_omitted", component="reporting",
                              clears=faults.CLASS_POLICY["report_omitted"]["clears"])
        with self.assertRaises(ValueError):
            faults.register_class("report_omitted", component="reporting",
                                  clears="something else entirely")

    def test_a_reading_this_cannot_read_is_named_rather_than_dropped(self):
        batch = faultsweep.sweep(self.store, readings=[
            {"schema": "something/else"}, {"schema": faultsweep.OBSERVATION_SCHEMA},
        ])
        self.assertEqual(2, len(batch["gaps"]))
        answer = faultsweep.record_all(self.ledger, batch, store=self.store)
        self.assertEqual(2, len(answer["gaps"]))

    def test_a_cursor_advances_only_after_the_rows_were_recorded(self):
        self.stall()
        batch = faultsweep.sweep(self.store)
        self.assertEqual([], self.store.all("SELECT source FROM fault_cursors"))
        faultsweep.record_all(self.ledger, batch, store=self.store)
        self.assertTrue(self.store.all("SELECT source FROM fault_cursors"))


class AnExistingStoreGainsTheFaultTables(RelayTestCase):
    """CREATE TABLE IF NOT EXISTS reaches a database that predates this work."""

    def test_a_database_without_the_fault_tables_gains_them_on_open_and_works(self):
        from codex_session_relay.store import Store

        with self.store.transaction() as db:
            for table in ("fault_ledger", "fault_occurrences", "fault_timeline",
                          "fault_remediations", "fault_publications", "fault_targets",
                          "fault_cursors"):
                db.execute(f"DROP TABLE {table}")
        path = str(self.store.path)
        self.store.close()
        reopened = Store(path)
        self.addCleanup(reopened.close)
        names = {row[0] for row in reopened.all(
            "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault%'")}
        # Seven from the first contract, ten from the corrected one; none of them altered.
        self.assertEqual(17, len(names))
        ledger = faults.FaultLedger(reopened, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        answer = ledger.record(omission("upgrade"))
        self.assertEqual(faults.OPEN, answer["state"])
        self.assertTrue(ledger.next())


class FifthReviewFindings(RelayTestCase):
    """Round five: a false positive that would have filed issues nobody should see."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.register()
        self.relationship = self.store.one(
            "SELECT relationship_id FROM relationships")["relationship_id"]

    def bind(self, generation=7, turn="turn-new"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR REPLACE INTO generations (relationship_id, execution_generation,"
                "  dispatch_request_id, anchor_state, dispatch_turn_id, opened_at)"
                " VALUES (?,?,?,?,?,?)",
                (self.relationship, generation, f"req-{generation}", "bound", turn,
                 self.clock.iso()))

    def poll(self, generation=7, turn="turn-new", polled=None, error=None):
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR REPLACE INTO poll_observations (relationship_id,"
                "  execution_generation, turn_id, last_status, last_polled_at,"
                "  last_attempt_at, last_error) VALUES (?,?,?,?,?,?,?)",
                (self.relationship, generation, turn, "inProgress", polled,
                 self.clock.iso(), error))


    def attempt(self, request_id, event_id, attempt_no=1,
                state="withheld_pre_send"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO attempts (request_id, event_id, attempt_no, kind,"
                "  internal_state, state, sent_at, observed_at)"
                " VALUES (?,?,?,?,?,?,?,?)",
                (request_id, event_id, attempt_no, "completion_event", "settled", state,
                 self.clock.now(), self.clock.iso()))

    def unheld(self, event_id="event-r", recipient="01parent-task"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event_id, self.relationship, "completion_event", recipient, recipient,
                 "queued", 2, None, self.clock.iso(), self.clock.iso()))

    def test_a_retrying_delivery_is_degraded_before_it_is_ever_capped(self):
        """The hold reason is only set at a cap, so requiring one made degraded unreachable."""
        self.unheld()
        for index in range(3):
            self.attempt(f"del-r-a{index}", "event-r", attempt_no=index + 1)
        derived = faultsweep.retry_faults(
            self.store, product=PRODUCT, scope={})["observations"]
        self.assertEqual(3, len(derived))
        self.assertEqual({faults.DEGRADED}, {entry["severity"] for entry in derived})
        self.assertEqual(1, len({faults.canonical_signature(entry["signature"])
                                 for entry in derived}))
        for entry in derived:
            self.ledger.record(entry)
        row = self.store.all("SELECT state, occurrence_count FROM fault_ledger")[0]
        self.assertEqual(3, row["occurrence_count"])
        self.assertEqual(faults.OPEN, row["state"])

    def test_one_occurrence_per_attempt_not_per_sweep(self):
        self.unheld()
        self.attempt("del-r-a0", "event-r")
        for _round in range(3):
            for entry in faultsweep.retry_faults(
                    self.store, product=PRODUCT, scope={})["observations"]:
                self.ledger.record(entry)
        self.assertEqual(1, self.store.all(
            "SELECT occurrence_count FROM fault_ledger")[0]["occurrence_count"])

    def test_an_anchor_bound_a_moment_ago_is_not_a_stalled_one(self):
        """It has not been due yet. Raising on that filed a fault for every new assignment."""
        self.bind()
        self.assertEqual([], faultsweep.observation_faults(
            self.store, product=PRODUCT, scope={})["observations"])

    def test_an_anchor_whose_attempts_have_never_succeeded_is_still_derived(self):
        self.bind()
        self.poll(error="boom")
        derived = faultsweep.observation_faults(
            self.store, product=PRODUCT, scope={})["observations"]
        self.assertEqual(1, len(derived))
        self.assertEqual(faults.BROKEN, derived[0]["severity"])

    def test_retargeting_a_scope_moves_writes_queued_against_the_old_tracker(self):
        self.ledger.set_target(product="crw", project="CRW", team="team-old", project_ref="proj-CRW")
        answer = self.ledger.record(omission("a"))
        self.assertEqual("team-old", self.store.one(
            "SELECT tracker_ref FROM fault_publications WHERE publication_id = ?",
            (answer["publication"]["publicationId"],))["tracker_ref"])
        moved = self.ledger.set_target(product="crw", project="CRW", team="team-new", project_ref="proj-CRW")
        self.assertEqual(1, moved["backfilled"])
        self.assertEqual("team-new", self.store.one(
            "SELECT tracker_ref FROM fault_publications WHERE publication_id = ?",
            (answer["publication"]["publicationId"],))["tracker_ref"])

    def test_a_reading_that_establishes_something_clears_the_unmeasured_notice(self):
        reading = {"schema": faultsweep.OBSERVATION_SCHEMA,
                   "relationshipId": self.relationship,
                   "selectors": {"turn": "turn-7"}, "reportingState": "unmeasured"}
        first = faultsweep.sweep(self.store, readings=[reading])
        faultsweep.record_all(self.ledger, first, store=self.store)
        identifier = faults.fault_id(PRODUCT, "observation_unmeasured",
                                     {"relationship": self.relationship, "turn": "turn-7"})
        self.assertIsNone(self.ledger.get(identifier)["cleared_at"])
        second = faultsweep.sweep(self.store, readings=[
            dict(reading, reportingState="reported")])
        faultsweep.record_all(self.ledger, second, store=self.store)
        self.assertIsNotNone(self.ledger.get(identifier)["cleared_at"])

    def test_a_still_running_turn_also_answers_the_unmeasured_question(self):
        """in_progress and unmanaged establish something too, so the notice does not linger."""
        reading = {"schema": faultsweep.OBSERVATION_SCHEMA,
                   "relationshipId": self.relationship,
                   "selectors": {"turn": "turn-7"}, "reportingState": "unmeasured"}
        faultsweep.record_all(self.ledger, faultsweep.sweep(self.store, readings=[reading]),
                              store=self.store)
        identifier = faults.fault_id(PRODUCT, "observation_unmeasured",
                                     {"relationship": self.relationship, "turn": "turn-7"})
        self.assertIsNone(self.ledger.get(identifier)["cleared_at"])
        faultsweep.record_all(self.ledger, faultsweep.sweep(
            self.store, readings=[dict(reading, reportingState="in_progress")]),
            store=self.store)
        self.assertIsNotNone(self.ledger.get(identifier)["cleared_at"])

    def test_a_notice_recorded_per_relationship_is_still_answered_by_a_reading(self):
        """Notices were per relationship before they were per turn; those rows still clear."""
        legacy = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="observation_unmeasured", severity=faults.NOTICE,
            signature={"relationship": self.relationship},
            occurrence_key=f"unmeasured:{self.relationship}:turn-1", scope=SCOPE))
        reading = {"schema": faultsweep.OBSERVATION_SCHEMA,
                   "relationshipId": self.relationship,
                   "selectors": {"turn": "turn-7"}, "reportingState": "reported"}
        faultsweep.record_all(self.ledger, faultsweep.sweep(self.store, readings=[reading]),
                              store=self.store)
        self.assertIsNotNone(self.ledger.get(legacy["faultId"])["cleared_at"])
        again = faultsweep.record_all(
            self.ledger, faultsweep.sweep(self.store, readings=[
                dict(reading, selectors={"turn": "turn-8"})]), store=self.store)
        self.assertEqual(0, again["recorded"], "the per-relationship clear is recorded once")

    def test_a_re_observed_occurrence_still_carries_a_scope_move(self):
        """Familiar occurrence, new project. The write must follow the fault, not the key."""
        self.ledger.set_target(product="crw", project="NEW", team="team-new", project_ref="proj-NEW")
        first = self.ledger.record(omission("same-key"))
        self.assertTrue(first["publication"]["awaitingTarget"])
        again = self.ledger.record(dict(omission("same-key"),
                                        scope={"projectKey": "NEW", "issueKey": "NEW-1"}))
        self.assertFalse(again["recorded"])
        self.assertEqual("crw:NEW", self.ledger.get(first["faultId"])["scope_key"])
        self.assertEqual("team-new", self.store.one(
            "SELECT tracker_ref FROM fault_publications WHERE publication_id = ?",
            (first["publication"]["publicationId"],))["tracker_ref"])

    def test_retargeting_reaches_a_failed_write_because_retry_reopens_it(self):
        self.ledger.set_target(product="crw", project="CRW", team="team-old", project_ref="proj-CRW")
        answer = self.ledger.record(omission("a"))
        publication = answer["publication"]["publicationId"]
        with self.store.transaction() as db:
            db.execute("UPDATE fault_publications SET state = ? WHERE publication_id = ?",
                       (faults.FAILED, publication))
        self.ledger.set_target(product="crw", project="CRW", team="team-new", project_ref="proj-CRW")
        self.ledger.retry(publication)
        self.assertEqual("team-new", self.store.one(
            "SELECT tracker_ref FROM fault_publications WHERE publication_id = ?",
            (publication,))["tracker_ref"])

    def test_malformed_json_at_the_command_line_is_a_refusal_not_a_traceback(self):
        import contextlib
        import io

        from codex_session_relay import cli

        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = cli.main(["--state", str(self.store.path.parent), "fault-observe",
                             "--observation", "{not json"])
        self.assertEqual(2, code)
        self.assertEqual("fault_observation_malformed",
                         json.loads(buffer.getvalue())["reason"])




class Lifecycle(LedgerCase):
    def setUp(self):
        super().setUp()
        self.identifier = self.ledger.record(omission("a"))["faultId"]
        self.publish(self.identifier, self.ledger.next()[0]["publication_id"])

    def test_a_fix_alone_does_not_resolve(self):
        self.ledger.record_fix(self.identifier, ref="PR #123")
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.resolve(self.identifier)
        self.assertEqual("fault_unverified", refusal.exception.reason.value)

    def test_a_verification_recorded_before_the_fix_it_claims_does_not_count(self):
        self.ledger.record_fix(self.identifier, ref="PR #1")
        self.ledger.record_reverification(self.identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.record_fix(self.identifier, ref="PR #2")
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.resolve(self.identifier)
        self.assertEqual("fault_verification_stale", refusal.exception.reason.value)

    def test_a_reverification_that_found_the_fault_does_not_resolve_it(self):
        self.ledger.record_fix(self.identifier, ref="PR #1")
        self.ledger.record_reverification(self.identifier, method="command", ref="relay show",
                                          outcome=faults.FAILED_CHECK)
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.resolve(self.identifier)
        self.assertEqual("fault_unverified", refusal.exception.reason.value)

    def test_an_outcome_outside_the_vocabulary_is_refused(self):
        self.ledger.record_fix(self.identifier, ref="PR #1")
        with self.assertRaises(faults.FaultRefused):
            self.ledger.record_reverification(self.identifier, method="suite", ref="pytest",
                                              outcome="looks fine to me")

    def test_an_occurrence_after_the_verification_refuses_the_resolution(self):
        self.ledger.record_fix(self.identifier, ref="PR #1")
        self.ledger.record_reverification(self.identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.record(omission("b", turn="turn-7"))
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.resolve(self.identifier)
        # The recurrence moved the fault out of fix_pending, which is the first thing resolve
        # checks. The guard BEHIND that check has its own test below.
        self.assertEqual("fault_state_conflict", refusal.exception.reason.value)

    def test_an_occurrence_after_the_verification_is_refused_on_its_own_terms(self):
        """The guard behind the state check, exercised where the state cannot mask it."""
        self.ledger.record_fix(self.identifier, ref="PR #1")
        self.ledger.record_reverification(self.identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,"
                "  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)",
                (self.identifier, 1, "occurrence", "later-occurrence", "it happened again",
                 self.clock.iso(), self.clock.now()))
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.resolve(self.identifier)
        self.assertEqual("fault_recurred_after_verification",
                         refusal.exception.reason.value)

    def test_a_misdated_observation_cannot_refuse_an_honest_verification(self):
        """Ordering is this store's sequence, so a caller's clock decides nothing."""
        misdated = omission("early", turn="turn-7")
        misdated["observedAt"] = "2099-01-01T00:00:00.000000+00:00"
        self.ledger.record(misdated)
        self.ledger.record_fix(self.identifier, ref="PR #1")
        self.ledger.record_reverification(self.identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.assertTrue(self.ledger.resolve(self.identifier)["resolved"])

    def test_a_recurrence_reopens_the_same_record_and_comments_on_the_same_issue(self):
        self.ledger.record_fix(self.identifier, ref="PR #1")
        self.ledger.record_reverification(self.identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.resolve(self.identifier)
        again = self.ledger.record(omission("later", turn="turn-7"))
        row = self.ledger.get(self.identifier)
        self.assertEqual(faults.OPEN, row["state"])
        self.assertEqual(2, row["cycle"])
        self.assertEqual(1, row["reopen_count"])
        self.assertEqual("REL-77", row["external_ref"])
        self.assertEqual(faults.APPEND_COMMENT, again["publication"]["kind"])
        self.assertEqual(
            1, self.store.one(
                "SELECT COUNT(*) AS n FROM fault_publications WHERE kind = ?",
                (faults.OPEN_RECORD,))["n"])


class Publication(LedgerCase):
    def test_one_fault_never_queues_a_second_create(self):
        """A trigger arriving before the create confirms is a comment, held until it can land."""
        identifier = self.ledger.record(omission("a"))["faultId"]
        self.ledger.record_fix(identifier, ref="PR #1")
        kinds = [row["kind"] for row in self.store.all(
            "SELECT kind FROM fault_publications WHERE fault_id = ?", (identifier,))]
        self.assertEqual([faults.OPEN_RECORD, faults.APPEND_COMMENT], kinds)
        offered = [job["kind"] for job in self.ledger.next()]
        self.assertEqual([faults.OPEN_RECORD], offered)

    def test_a_comment_cannot_be_claimed_before_the_issue_exists(self):
        identifier = self.ledger.record(omission("a"))["faultId"]
        fix = self.ledger.record_fix(identifier, ref="PR #1")
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.claim(fix["publication"]["publicationId"], owner="operator")
        self.assertEqual("fault_not_claimable", refusal.exception.reason.value)

    def test_one_reason_queues_one_write_however_often_it_is_swept(self):
        identifier = self.ledger.record(omission("a"))["faultId"]
        first = self.ledger.record_fix(identifier, ref="PR #1")
        second = self.ledger.record_fix(identifier, ref="PR #1")
        self.assertTrue(first["publication"]["queued"])
        self.assertFalse(second["recorded"])

    def test_a_lost_response_after_the_write_was_issued_never_creates_twice(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.fail(job, claim_token=claim["claimToken"], error="the response was lost")
        self.assertEqual(faults.UNCERTAIN, self.store.one(
            "SELECT state FROM fault_publications WHERE publication_id = ?", (job,))["state"])
        self.assertEqual([], self.ledger.next())
        with self.assertRaises(faults.FaultRefused):
            self.ledger.claim(job, owner="somebody else")

    def test_an_expiring_lease_releases_a_claim_but_never_an_issued_write(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.assertEqual({"released": 1, "uncertain": 0}, self.ledger.expire_leases())
        claim = self.ledger.claim(job, owner="operator")
        self.ledger.operation(job, claim_token=claim["claimToken"])
        self.clock.advance(faults.LEASE_SECONDS + 1)
        self.assertEqual({"released": 0, "uncertain": 1}, self.ledger.expire_leases())
        self.assertEqual([], self.ledger.next())

    def test_an_uncertain_write_is_confirmed_from_what_was_observed(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.fail(job, claim_token=claim["claimToken"], error="timeout")
        found = self.ledger.reconcile(job, "issue body\n" + operation["block"])
        self.assertEqual("present", found["outcome"])
        done = self.ledger.complete(job, project_ref="proj-CRW", readback="issue body\n" + operation["block"],
                                    external_ref="REL-9")
        self.assertTrue(done["confirmed"])
        self.assertEqual("REL-9", self.ledger.get(done["fault_id"])["external_ref"])

    def test_a_negative_read_permits_another_write_only_when_somebody_attests_it(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.fail(job, claim_token=claim["claimToken"], error="timeout")
        unattested = self.ledger.reconcile(job, "nothing here")
        self.assertEqual("absent_unattested", unattested["outcome"])
        self.assertEqual([], self.ledger.next())
        # An attested search is not enough on its own: the request may still land after it.
        unproven = self.ledger.reconcile(job, "nothing here", searched=True)
        self.assertEqual("absent_unproven", unproven["outcome"])
        self.assertEqual([], self.ledger.next())
        attested = self.ledger.reconcile(job, "nothing here", searched=True, prior_ended=True,
                                         reason="the connector answered 400")
        self.assertEqual("absent", attested["outcome"])
        self.assertEqual([job], [row["publication_id"] for row in self.ledger.next()])

    def test_a_readback_that_is_not_this_block_is_refused(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        damaged = operation["block"].replace("faultClass: report_omitted",
                                             "faultClass: something_else")
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.complete(job, project_ref="proj-CRW", claim_token=claim["claimToken"], readback=damaged,
                                 external_ref="REL-1")
        self.assertEqual("fault_readback_mismatch", refusal.exception.reason.value)


    def test_a_repeat_under_a_familiar_key_still_reopens_what_was_closed(self):
        """A fault that is happening again is happening again, whatever key it arrives under."""
        first = self.ledger.record(omission("same"))
        identifier = first["faultId"]
        self.publish(identifier, self.ledger.next()[0]["publication_id"])
        self.ledger.record_fix(identifier, ref="PR #1")
        self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.resolve(identifier)
        self.assertEqual(faults.RESOLVED, self.ledger.get(identifier)["state"])
        again = self.ledger.record(omission("same"))
        self.assertTrue(again["recorded"], "a new episode, so a new occurrence")
        row = self.ledger.get(identifier)
        self.assertEqual(faults.OPEN, row["state"])
        self.assertEqual(2, row["cycle"])
        self.assertEqual(2, row["occurrence_count"])
        self.assertEqual(faults.APPEND_COMMENT, again["publication"]["kind"])

    def test_a_withdrawn_fault_reopens_when_its_cause_is_observed_again(self):
        answer = self.ledger.record(stall("delivery:1"))
        self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="delivery_stalled", severity=faults.NOTICE,
            signature={"recipient": "01parent-task", "cause": "channel_closed",
                       "attemptState": "withheld_pre_send"},
            occurrence_key="cleared:1", scope=SCOPE, cleared=True))
        self.assertEqual(faults.WITHDRAWN, self.ledger.get(answer["faultId"])["state"])
        back = self.ledger.record(stall("delivery:1"))
        self.assertTrue(back["recorded"], "a new episode, so a new occurrence")
        self.assertEqual(faults.OBSERVED, self.ledger.get(answer["faultId"])["state"])

    def test_an_attested_absence_does_not_release_a_claim_somebody_still_holds(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        answer = self.ledger.reconcile(job, "nothing here", searched=True)
        self.assertEqual("absent", answer["outcome"])
        self.assertEqual(faults.CLAIMED, answer["state"])
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        self.assertTrue(operation["block"])

    def test_a_fault_with_no_configured_target_waits_instead_of_being_filed(self):
        ledger = faults.FaultLedger(self.store, self.clock)
        answer = ledger.record(faults.observation(
            product=PRODUCT, fault_class="report_omitted", severity=faults.BROKEN,
            signature={"relationship": "rel-9", "turn": "t"}, occurrence_key="k",
            scope={"projectKey": "OTHER"}))
        self.assertTrue(answer["publication"]["awaitingTarget"])
        self.assertEqual([], [job for job in ledger.next()
                              if job["fault_id"] == answer["faultId"]])
        ledger.set_target(product="crw", project="OTHER", team="team-other", project_ref="proj-OTHER")
        self.assertIn(answer["faultId"], [job["fault_id"] for job in ledger.next()])


class Sweep(RelayTestCase):
    """The sweep derives what the store shows, and clears by reading the source again."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")

    def stall_a_delivery(self, event_id, *, recipient="01parent-task"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event_id, "rel-1", "completion_event", recipient, recipient, "queued", 1,
                 "channel_closed", self.clock.iso(), self.clock.iso()),
            )

    def drain_deliveries(self):
        with self.store.transaction() as db:
            db.execute("UPDATE deliveries SET hold_reason = NULL, state = 'dispatched'")

    def sweep(self, readings=()):
        batch = faultsweep.sweep(self.store, scope={"projectKey": "CRW"}, readings=readings)
        return batch, faultsweep.record_all(self.ledger, batch, store=self.store)

    def test_two_stranded_deliveries_produce_one_fault_with_two_occurrences(self):
        self.stall_a_delivery("event-a")
        self.stall_a_delivery("event-b")
        _batch, recorded = self.sweep()
        self.assertEqual(2, recorded["recorded"])
        rows = self.store.all("SELECT fault_id, occurrence_count FROM fault_ledger")
        self.assertEqual(1, len(rows))
        self.assertEqual(2, rows[0]["occurrence_count"])

    def test_sweeping_the_same_unchanged_rows_again_records_nothing(self):
        self.stall_a_delivery("event-a")
        self.sweep()
        _batch, second = self.sweep()
        self.assertEqual(0, second["recorded"])
        self.assertEqual(0, second["queued"])

    def test_a_recipient_whose_deliveries_drained_is_cleared_by_re_reading_the_source(self):
        self.stall_a_delivery("event-a")
        self.sweep()
        self.drain_deliveries()
        batch, recorded = self.sweep()
        self.assertEqual(1, len(batch["clears"]))
        self.assertEqual(1, recorded["recorded"])
        row = self.store.all("SELECT state, cleared_at FROM fault_ledger")[0]
        self.assertEqual(faults.WITHDRAWN, row["state"])
        self.assertIsNotNone(row["cleared_at"])

    def test_an_unmeasured_reading_never_clears_an_omission(self):
        unreported = {"schema": faultsweep.OBSERVATION_SCHEMA, "relationshipId": "rel-1",
                      "selectors": {"turn": "turn-7"}, "reportingState": "unreported"}
        self.sweep(readings=[unreported])
        identifier = faults.fault_id(PRODUCT, "report_omitted",
                                     {"relationship": "rel-1", "turn": "turn-7"})
        self.assertEqual(faults.OPEN, self.ledger.get(identifier)["state"])
        unmeasured = dict(unreported, reportingState="unmeasured")
        self.sweep(readings=[unmeasured])
        self.assertEqual(faults.OPEN, self.ledger.get(identifier)["state"])
        self.assertIsNone(self.ledger.get(identifier)["cleared_at"])

    def test_a_reading_that_found_the_report_clears_the_omission(self):
        unreported = {"schema": faultsweep.OBSERVATION_SCHEMA, "relationshipId": "rel-1",
                      "selectors": {"turn": "turn-7"}, "reportingState": "unreported"}
        self.sweep(readings=[unreported])
        self.sweep(readings=[dict(unreported, reportingState="reported")])
        identifier = faults.fault_id(PRODUCT, "report_omitted",
                                     {"relationship": "rel-1", "turn": "turn-7"})
        self.assertIsNotNone(self.ledger.get(identifier)["cleared_at"])

    def test_a_class_the_sweep_does_not_derive_is_never_cleared_by_its_absence(self):
        unreported = {"schema": faultsweep.OBSERVATION_SCHEMA, "relationshipId": "rel-1",
                      "selectors": {"turn": "turn-7"}, "reportingState": "unreported"}
        self.sweep(readings=[unreported])
        batch, _recorded = self.sweep()
        self.assertEqual([], [entry for entry in batch["clears"]
                              if entry["faultClass"] == "report_omitted"])

    def test_every_source_query_is_bounded(self):
        for index in range(faultsweep.SWEEP_LIMIT + 5):
            self.stall_a_delivery(f"event-{index}", recipient=f"task-{index}")
        batch = faultsweep.sweep(self.store, scope={"projectKey": "CRW"})
        self.assertEqual(faultsweep.SWEEP_LIMIT, len(batch["observations"]))
        self.assertNotIn("delivery_stalled", batch["completeSources"])


class DaemonPass(DeliveryTestCase):
    """A real daemon with real collaborators, so the tick under test is the tick that ships."""

    def daemon(self, ledger):
        from codex_session_relay.daemon import RelayDaemon

        return RelayDaemon(self.store, self.registry, self.intake, self.delivery, self.ack,
                           self.reconciler, self.adapter, clock=self.clock, faults=ledger,
                           fault_scope={"projectKey": "CRW"})

    def test_the_tick_records_what_is_broken_and_goes_quiet_once_it_has(self):
        from codex_session_relay.daemon import TickReport

        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                ("event-a", "rel-1", "completion_event", "01parent-task", "01parent-task",
                 "queued", 1, "channel_closed", self.clock.iso(), self.clock.iso()),
            )
        daemon = self.daemon(ledger)
        first = TickReport()
        daemon._sweep_faults(first)
        self.assertEqual(1, first.faultsRecorded)
        second = TickReport()
        daemon._sweep_faults(second)
        self.assertEqual(0, second.faultsRecorded)
        self.assertEqual([], second.notes)

    def test_a_daemon_without_a_ledger_ticks_exactly_as_it_did(self):
        from codex_session_relay.daemon import TickReport

        report = TickReport()
        self.daemon(None)._sweep_faults(report)
        self.assertEqual(0, report.faultsRecorded)
        self.assertEqual([], report.notes)

    def test_recorded_faults_keep_a_tick_from_being_called_quiet(self):
        """Through the real tick, so this fails if tick() stops folding the counter in."""
        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                ("event-quiet", "rel-1", "completion_event", "01parent-task",
                 "01parent-task", "queued", 1, "channel_closed", self.clock.iso(),
                 self.clock.iso()))
        report = self.daemon(ledger).tick()
        self.assertEqual(1, report.faultsRecorded)
        self.assertFalse(report.quiet)

    def test_a_selection_given_to_the_daemon_reaches_the_managed_readings(self):
        """B5: the daemon reads its own managed turns only if it is handed the selection."""
        import inspect
        from unittest import mock

        from codex_session_relay.daemon import RelayDaemon, TickReport

        self.assertIn("fault_selection", inspect.signature(RelayDaemon).parameters,
                      "the daemon cannot be given the selection omitted.observe reads through")
        seen = []

        def readings(_store, selection, **_kw):
            seen.append(selection)
            return {"readings": [], "gaps": [], "cursor": None, "filled": False,
                    "complete": True}

        daemon = RelayDaemon(self.store, self.registry, self.intake, self.delivery, self.ack,
                             self.reconciler, self.adapter, clock=self.clock,
                             faults=faults.FaultLedger(self.store, self.clock),
                             fault_selection="the-selection")
        with mock.patch.object(faultsweep, "managed_readings", readings):
            daemon._sweep_faults(TickReport())
        self.assertEqual(["the-selection"], seen)

    def test_unsent_writes_are_a_note_when_they_appear_and_again_when_they_change(self):
        """B11: a tick carries the attention warning, once per change rather than every tick."""
        from codex_session_relay.daemon import TickReport

        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        self.assertTrue(callable(getattr(ledger, "attention", None)),
                        "the ledger offers no attention()")
        daemon = self.daemon(ledger)
        ledger.record(omission("a"))
        first = TickReport()
        daemon._sweep_faults(first)
        self.assertIn(ledger.attention()["warning"], first.notes)
        second = TickReport()
        daemon._sweep_faults(second)
        self.assertEqual([], second.notes)
        publication = ledger.next()[0]["publication_id"]
        ledger.claim(publication, owner="operator")
        third = TickReport()
        daemon._sweep_faults(third)
        self.assertIn(ledger.attention()["warning"], third.notes)


class CommandLine(RelayTestCase):
    """In process, so this module measures no wall time and starts no subprocess."""

    def invoke(self, *argv):
        """Named away from run(), which unittest calls with a result it would swallow."""
        import contextlib
        import io

        from codex_session_relay import cli

        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = cli.main(["--state", str(self.store.path.parent), *argv])
        return code, json.loads(buffer.getvalue())

    def test_a_fault_is_observed_filed_and_carried_to_confirmed_from_the_command_line(self):
        code, _ = self.invoke("fault-target", "--product", "crw", "--project", "CRW", "--team", TRACKER, "--project-ref", "proj-CRW")
        self.assertEqual(0, code)
        code, recorded = self.invoke("fault-observe", "--observation",
                                  json.dumps(omission("cli-1")))
        self.assertEqual(0, code)
        self.assertEqual(faults.OPEN, recorded["state"])
        code, queue = self.invoke("fault-next")
        publication = queue["publications"][0]["publication_id"]
        code, claim = self.invoke("fault-claim", "--publication", publication,
                               "--owner", "operator")
        code, operation = self.invoke("fault-operation", "--publication", publication,
                                   "--claim-token", claim["claimToken"])
        readback = self.artifact("readback.md", "issue\n" + operation["block"])
        code, done = self.invoke("fault-complete", "--publication", publication,
                              "--claim-token", claim["claimToken"],
                              "--readback", "@" + readback, "--external-ref", "REL-5",
                              "--project-ref", "proj-CRW")
        self.assertEqual(0, code)
        self.assertTrue(done["confirmed"])
        code, shown = self.invoke("fault-show", "--fault", recorded["faultId"])
        self.assertEqual("REL-5", shown["external_ref"])

    def test_the_command_line_refuses_to_resolve_an_unverified_fault(self):
        self.invoke("fault-target", "--product", "crw", "--project", "CRW", "--team", TRACKER, "--project-ref", "proj-CRW")
        _code, recorded = self.invoke("fault-observe", "--observation",
                                   json.dumps(omission("cli-1")))
        self.invoke("fault-fix", "--fault", recorded["faultId"], "--ref", "PR #1")
        code, refusal = self.invoke("fault-resolve", "--fault", recorded["faultId"])
        self.assertEqual(2, code)
        self.assertEqual("fault_unverified", refusal["reason"])

    NEW_FAULT_COMMANDS = (
        "fault-adopt", "fault-move", "fault-queue", "fault-update", "fault-cancel",
        "fault-stage", "fault-policy", "fault-limit", "fault-attention", "fault-relink",
        "fault-notifications", "fault-notification-raise", "fault-notification-reserve",
        "fault-notification-ack", "fault-notification-fail", "fault-notification-reconcile",
    )

    def offers(self, command, flag):
        """The command line takes this flag, or a failure saying it does not."""
        import argparse

        from codex_session_relay import cli

        parser = cli.build_parser()
        if command is not None:
            action = next(a for a in parser._actions
                          if isinstance(a, argparse._SubParsersAction))
            parser = action.choices[command]
        flags = {option for action in parser._actions for option in action.option_strings}
        self.assertIn(flag, flags, f"{command or 'the relay'} takes no {flag}")

    def test_every_new_fault_command_has_a_handler_and_runs_offline(self):
        from codex_session_relay import cli

        choices = cli.build_parser()._subparsers._group_actions[0].choices
        for name in self.NEW_FAULT_COMMANDS:
            self.assertIn(name, choices, name)
            self.assertTrue(callable(choices[name].get_default("handler")), name)
            self.assertIn(name, cli.OFFLINE_COMMANDS, name)

    def test_status_shows_the_unsent_fault_writes(self):
        """B11: a write waiting for a target is visible where status is read."""
        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER)
        ledger.record(omission("cli-1"))
        code, status = self.invoke("status")
        self.assertEqual(0, code)
        self.assertIn("faults", status)
        self.assertEqual(1, status["faults"]["unsent"]["awaitingTarget"])
        self.assertIn("awaitingTarget", status["faults"]["warning"])

    def test_fault_next_says_why_each_write_waits(self):
        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER)
        ledger.record(omission("cli-1"))
        code, queue = self.invoke("fault-next")
        self.assertEqual(0, code)
        self.assertIn("held", queue, "the queue does not say why a write waits")
        self.assertEqual([], queue["publications"])
        self.assertEqual(["awaiting_target"], [entry["reason"] for entry in queue["held"]])

    def test_one_publication_is_shown_with_its_attempts(self):
        self.offers("fault-show", "--publication")
        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        ledger.record(omission("cli-1"))
        publication = ledger.next()[0]["publication_id"]
        ledger.claim(publication, owner="operator")
        code, shown = self.invoke("fault-show", "--publication", publication)
        self.assertEqual(0, code)
        self.assertEqual(publication, shown["publication_id"])
        self.assertEqual(["operator"], [attempt["owner"] for attempt in shown["attempts"]])

    def test_the_sweep_command_continues_a_readings_batch(self):
        self.offers("fault-sweep", "--readings-after")
        self.offers("fault-show", "--fault-class")
        readings = [{"schema": faultsweep.OBSERVATION_SCHEMA, "relationshipId": f"rel-{n}",
                     "selectors": {"turn": "turn-1"}, "reportingState": "unreported"}
                    for n in range(3)]
        path = self.artifact("readings.json", json.dumps(readings))
        code, swept = self.invoke("fault-sweep", "--readings", "@" + path,
                                  "--readings-after", "2")
        self.assertEqual(0, code)
        self.assertEqual(3, swept["readingsTotal"])
        self.assertIsNone(swept["readingsNext"])
        code, listing = self.invoke("fault-show", "--fault-class", "report_omitted")
        self.assertEqual([{"relationship": "rel-2", "turn": "turn-1"}],
                         [json.loads(row["signature"]) for row in listing["faults"]])

    def test_a_kind_module_is_imported_before_the_command_runs(self):
        import sys

        from codex_session_relay import cli

        self.offers(None, "--kind-module")
        name = "crw205_cli_probe_kind"
        module = f"crw205_cli_probe_module_{id(self)}"
        directory = pathlib.Path(self.artifact(module + ".py", (
            "from codex_session_relay import faults\n"
            f"faults.register_kind({name!r}, creates=False, requires_issue=False,"
            " target=None, evidence='fields', confirm=lambda expected, observed: [])\n"
        ))).parent
        sys.path.insert(0, str(directory))
        self.addCleanup(sys.path.remove, str(directory))
        self.addCleanup(sys.modules.pop, module, None)
        self.addCleanup(faults.KINDS.pop, name, None)
        _code, recorded = self.invoke("fault-observe", "--observation", json.dumps(
            faults.observation(product=PRODUCT, fault_class="observation_unmeasured",
                               severity=faults.NOTICE, signature={"relationship": "rel-1"},
                               occurrence_key="u1", scope=SCOPE)))
        self.assertNotIn(name, faults.KINDS)
        code, queued = self.invoke("--kind-module", module, "fault-queue", "--fault",
                                   recorded["faultId"], "--kind", name, "--trigger", "t1")
        self.assertEqual(0, code, queued)
        self.assertTrue(queued["queued"])
        code, refusal = self.invoke("--kind-module", "no_such_module_crw205", "fault-attention")
        self.assertEqual(cli.EXIT_USAGE, code)
        self.assertIn("no_such_module_crw205", refusal["detail"])


class TheFaultPathReachesNoNetwork(unittest.TestCase):
    """Asserted from the source, because a boundary nobody checks is a boundary that moves."""

    FORBIDDEN = {"socket", "ssl", "http", "http.client", "urllib", "urllib.request",
                 "requests", "httpx", "asyncio", "smtplib", "ftplib", "xmlrpc"}

    def test_no_module_on_the_fault_path_imports_a_network_client(self):
        checked = 0
        for name in ("faults.py", "faultsweep.py"):
            tree = ast.parse((SOURCE / name).read_text(encoding="utf-8"))
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    for alias in node.names:
                        self.assertNotIn(alias.name.split(".")[0], self.FORBIDDEN, name)
                if isinstance(node, ast.ImportFrom) and node.module:
                    self.assertNotIn(node.module.split(".")[0], self.FORBIDDEN, name)
            checked += 1
        self.assertEqual(2, checked)

    def test_every_registered_class_declares_what_clears_it(self):
        self.assertTrue(faults.CLASS_POLICY)
        for name, policy in faults.CLASS_POLICY.items():
            self.assertTrue(policy["clears"], name)


class NoSubcommandShadowsAGlobalOption(unittest.TestCase):
    """The bug this closes was mine, and it is closed for every subcommand, not just for one.

    fault-show took --state as a filter. argparse puts a subcommand's options into the SAME
    namespace as the parser's own, so that name overwrote the global --state naming the store
    directory: every fault-show silently read the default store and answered that the fault
    did not exist. Nothing failed; it just looked empty.
    """

    def test_no_subparser_redefines_an_option_the_top_level_parser_owns(self):
        import argparse

        from codex_session_relay import cli

        parser = cli.build_parser()
        top = {option for action in parser._actions for option in action.option_strings}
        subparsers = [action for action in parser._actions
                      if isinstance(action, argparse._SubParsersAction)]
        self.assertTrue(subparsers, "the CLI exposes no subcommands")
        checked = 0
        for action in subparsers:
            for name, sub in action.choices.items():
                for option in (o for a in sub._actions for o in a.option_strings):
                    if option in ("-h", "--help"):
                        continue
                    self.assertNotIn(
                        option, top,
                        f"{name} redefines the global option {option}, which overwrites it",
                    )
                checked += 1
        self.assertGreater(checked, 40, f"only {checked} subcommands were checked")


class ReviewFindings(RelayTestCase):
    """Each of these is a defect an independent review found. Each keeps its own test."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)

    def register_scope(self, project="CRW"):
        """A real registered relationship, placed in a project, as the registry writes it."""
        self.register()
        relationship = self.store.one("SELECT relationship_id FROM relationships")[
            "relationship_id"]
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR REPLACE INTO relationship_scope (relationship_id, project_key,"
                "  recorded_at) VALUES (?,?,?)", (relationship, project, self.clock.iso()))
        return relationship

    def stall(self, event_id, *, relationship, recipient="01parent-task"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event_id, relationship, "completion_event", recipient, recipient, "queued", 1,
                 "channel_closed", self.clock.iso(), self.clock.iso()),
            )

    def test_a_fault_is_filed_in_the_project_its_relationship_belongs_to(self):
        """A daemon sweeps a store holding several projects and passes no scope of its own."""
        relationship = self.register_scope(project="CRW")
        self.stall("event-a", relationship=relationship)
        batch = faultsweep.sweep(self.store)
        self.assertEqual("CRW", batch["observations"][0]["scope"]["projectKey"])
        faultsweep.record_all(self.ledger, batch, store=self.store)
        row = self.store.all("SELECT scope_key FROM fault_ledger")[0]
        self.assertEqual("crw:CRW", row["scope_key"])

    def test_a_source_that_filled_its_page_clears_nothing(self):
        """A truncated read establishes nothing about absence, so it may not withdraw a fault."""
        relationship = self.register_scope()
        for index in range(faultsweep.SWEEP_LIMIT):
            self.stall(f"event-{index}", relationship=relationship,
                       recipient=f"task-{index}")
        first = faultsweep.sweep(self.store)
        self.assertNotIn("delivery_stalled", first["completeSources"])
        faultsweep.record_all(self.ledger, first)
        self.stall("event-extra", relationship=relationship,
                   recipient="task-extra")
        second = faultsweep.sweep(self.store)
        self.assertEqual([], second["clears"])
        self.assertEqual(
            0, self.store.one("SELECT COUNT(*) AS n FROM fault_ledger WHERE state = ?",
                              (faults.WITHDRAWN,))["n"])

    def test_a_create_cannot_be_confirmed_without_naming_the_issue_it_created(self):
        """Otherwise the ledger owns no issue and every later comment waits forever."""
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.complete(job, project_ref="proj-CRW", claim_token=claim["claimToken"],
                                 readback=operation["block"])
        self.assertEqual("fault_readback_mismatch", refusal.exception.reason.value)

    def test_an_old_poll_of_another_turn_does_not_make_a_healthy_anchor_look_stalled(self):
        relationship = self.register_scope()
        with self.store.transaction() as db:
            db.execute(
                "DELETE FROM poll_observations")
            db.execute(
                "INSERT OR REPLACE INTO generations (relationship_id, execution_generation,"
                "  dispatch_request_id, anchor_state, dispatch_turn_id, opened_at)"
                " VALUES (?,?,?,?,?,?)",
                (relationship, 9, "req-fault-1", "bound", "turn-current",
                 self.clock.iso()))
            db.execute(
                "INSERT INTO poll_observations (relationship_id, execution_generation,"
                "  turn_id, last_status, last_polled_at, last_attempt_at, last_error)"
                " VALUES (?,?,?,?,?,?,?)",
                (relationship, 9, "turn-old", "failed", None, self.clock.iso(), "boom"))
            db.execute(
                "INSERT INTO poll_observations (relationship_id, execution_generation,"
                "  turn_id, last_status, last_polled_at, last_attempt_at, last_error)"
                " VALUES (?,?,?,?,?,?,?)",
                (relationship, 9, "turn-current", "inProgress", self.clock.iso(),
                 self.clock.iso(), None))
        # Generation 9's current anchor polls fine and its only failure belongs to a turn the
        # scheduler has moved on from, so it raises nothing. The registry's own generation has
        # never been ATTEMPTED, which is a new assignment rather than a stalled one.
        stalled = faultsweep.observation_faults(
            self.store, product=PRODUCT, scope={})["observations"]
        self.assertEqual([], stalled)


class SecondReviewFindings(LedgerCase):
    """Round two of the same independent review. Each finding keeps its own test."""

    def test_claiming_directly_still_honours_the_backoff_the_queue_computed(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        self.ledger.fail(job, claim_token=claim["claimToken"], error="transient")
        self.assertEqual([], self.ledger.next())
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.claim(job, owner="an impatient caller")
        self.assertEqual("fault_not_claimable", refusal.exception.reason.value)
        self.clock.advance(faults.BASE_BACKOFF + 1)
        self.assertTrue(self.ledger.claim(job, owner="operator")["claimToken"])

    def test_a_write_established_absent_cannot_be_confirmed_from_the_earlier_readback(self):
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        stale = "issue body\n" + operation["block"]
        self.ledger.fail(job, claim_token=claim["claimToken"], error="the response was lost")
        self.assertEqual("absent", self.ledger.reconcile(
            job, "nothing here", searched=True, prior_ended=True,
            reason="the connector answered 400")["outcome"])
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.complete(job, project_ref="proj-CRW", readback=stale, external_ref="REL-1")
        self.assertEqual("fault_state_conflict", refusal.exception.reason.value)

    def test_a_queued_comment_is_written_against_the_cycle_it_was_queued_in(self):
        identifier = self.ledger.record(omission("a"))["faultId"]
        self.publish(identifier, self.ledger.next()[0]["publication_id"])
        self.ledger.record_fix(identifier, ref="PR #1")
        self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.resolve(identifier)
        queued = self.ledger.next()[0]["publication_id"]
        # The fault reopens before anybody gets round to writing the resolve comment.
        self.ledger.record(omission("later", turn="turn-7"))
        self.assertEqual(2, self.ledger.get(identifier)["cycle"])
        claim = self.ledger.claim(queued, owner="operator")
        operation = self.ledger.operation(queued, claim_token=claim["claimToken"])
        self.assertIn("cycle: 1", operation["block"])
        done = self.ledger.complete(queued, project_ref="proj-CRW", claim_token=claim["claimToken"],
                                    readback=operation["block"], external_ref="REL-77")
        self.assertTrue(done["confirmed"])

    def test_a_fault_suppression_never_filed_is_not_filed_by_recording_a_fix(self):
        answer = self.ledger.record(faults.observation(
            product=PRODUCT, fault_class="observation_unmeasured", severity=faults.NOTICE,
            signature={"relationship": "rel-1"}, occurrence_key="u1", scope=SCOPE))
        self.assertIsNone(answer["publication"])
        fix = self.ledger.record_fix(answer["faultId"], ref="PR #1")
        self.assertFalse(fix["publication"]["queued"])
        self.assertIsNone(fix["publication"]["publicationId"])
        self.assertEqual(
            0, self.store.one("SELECT COUNT(*) AS n FROM fault_publications")["n"])
        self.assertEqual([], self.ledger.next())





class SixthReviewFindings(RelayTestCase):
    """Round eight: the edges my own episode fix opened."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        self.register()
        self.relationship = self.store.one(
            "SELECT relationship_id FROM relationships")["relationship_id"]

    def publish(self, identifier):
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.complete(job, project_ref="proj-CRW", claim_token=claim["claimToken"],
                             readback=operation["block"], external_ref="REL-77")

    def test_a_recurrence_is_counted_once_however_many_sweeps_follow_it(self):
        """The episode is in the unique KEY, or every post-recovery sweep counts again."""
        first = self.ledger.record(omission("same"))
        identifier = first["faultId"]
        self.publish(identifier)
        self.ledger.record_fix(identifier, ref="PR #1")
        self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.resolve(identifier)
        counts = []
        for _sweep in range(4):
            self.ledger.record(omission("same"))
            counts.append(self.ledger.get(identifier)["occurrence_count"])
        self.assertEqual([2, 2, 2, 2], counts)
        self.assertEqual(2, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_occurrences")["n"])
        self.assertEqual(2, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_timeline WHERE kind = 'occurrence'")["n"])

    def test_an_identical_clearing_reading_does_not_open_an_episode_each_time(self):
        first = self.ledger.record(omission("a"))
        self.publish(first["faultId"])
        episodes = []
        for _sweep in range(3):
            self.ledger.record(omission("a:cleared", cleared=True))
            episodes.append(self.ledger.get(first["faultId"])["episode"])
        self.assertEqual([1, 1, 1], episodes)
        # A clear is not an occurrence: the count is the one thing that went wrong.
        self.assertEqual(1, self.ledger.get(first["faultId"])["occurrence_count"])

    def test_pruning_evidence_cannot_make_a_familiar_occurrence_look_new(self):
        first = self.ledger.record(omission("a"))
        self.ledger.record(omission("b"))
        self.ledger.prune(first["faultId"], keep=1)
        again = self.ledger.record(omission("a"))
        self.assertFalse(again["recorded"])
        self.assertEqual(2, self.ledger.get(first["faultId"])["occurrence_count"])

    def test_every_retry_failure_is_reached_by_rotation_not_only_the_newest_page(self):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
                "  recipient_thread_id, state, attempt_count, hold_reason, created_at,"
                "  updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                ("event-r", self.relationship, "completion_event", "01parent-task",
                 "01parent-task", "queued", 40, None, self.clock.iso(), self.clock.iso()))
            for index in range(faultsweep.SWEEP_LIMIT + 6):
                db.execute(
                    "INSERT INTO attempts (request_id, event_id, attempt_no, kind,"
                    "  internal_state, state, sent_at, observed_at)"
                    " VALUES (?,?,?,?,?,?,?,?)",
                    (f"del-r-a{index:03d}", "event-r", index + 1, "completion_event",
                     "settled", "withheld_pre_send", self.clock.now(), self.clock.iso()))
        seen = set()
        for _round in range(3):
            batch = faultsweep.sweep(self.store)
            faultsweep.record_all(self.ledger, batch, store=self.store)
            seen.update(entry["occurrenceKey"] for entry in batch["observations"]
                        if entry["faultClass"] == "delivery_stalled")
        self.assertEqual(faultsweep.SWEEP_LIMIT + 6, len(seen))

    def test_a_clear_under_the_same_key_it_raised_still_closes_the_fault(self):
        """Nothing forces an adapter to suffix its clears; without the direction in identity
        such a clear collided with the observation it was meant to close."""
        raised = self.ledger.record(omission("obs:rel-1:turn-7"))
        self.assertEqual(faults.OPEN, raised["state"])
        cleared = self.ledger.record(omission("obs:rel-1:turn-7", cleared=True))
        self.assertTrue(cleared["recorded"])
        self.assertIsNotNone(self.ledger.get(raised["faultId"])["cleared_at"])

    def test_the_recurrence_after_a_reported_clear_and_a_resolution_reopens(self):
        """The sequence round eight asked about, pinned so it cannot regress."""
        raised = self.ledger.record(omission("observation:rel-1:turn-7"))
        identifier = raised["faultId"]
        self.publish(identifier)
        self.ledger.record_fix(identifier, ref="PR #1")
        self.ledger.record(omission("observation:rel-1:turn-7:reported", cleared=True))
        self.ledger.record_reverification(identifier, method="observation",
                                          ref="relay reporting-show", outcome=faults.ABSENT)
        self.assertTrue(self.ledger.resolve(identifier)["resolved"])
        again = self.ledger.record(omission("observation:rel-1:turn-7"))
        self.assertTrue(again["recorded"], "the omission is active again")
        row = self.ledger.get(identifier)
        self.assertEqual(faults.OPEN, row["state"])
        self.assertEqual(2, row["cycle"])

    def test_a_limit_that_does_not_bound_anything_is_refused(self):
        self.ledger.record(omission("a"))
        with self.assertRaises(faults.FaultRefused):
            self.ledger.next(limit=-1)
        with self.assertRaises(faults.FaultRefused):
            self.ledger.occurrences(self.ledger.get(
                faults.fault_id(PRODUCT, "report_omitted",
                                {"relationship": "rel-1", "turn": "turn-7"}))["fault_id"],
                limit=0)

    def test_a_reading_state_this_sweep_cannot_interpret_is_named(self):
        batch = faultsweep.sweep(self.store, readings=[{
            "schema": faultsweep.OBSERVATION_SCHEMA, "relationshipId": self.relationship,
            "selectors": {"turn": "turn-7"}, "reportingState": "something_new"}])
        self.assertEqual([], batch["observations"])
        self.assertEqual(1, len(batch["gaps"]))
        self.assertEqual("reading_unknown_state", batch["gaps"][0]["gap"])


class SeventhReviewFindings(RelayTestCase):
    """Round four of the code review, on the bytes it actually read."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        self.register()
        self.relationship = self.store.one(
            "SELECT relationship_id FROM relationships")["relationship_id"]

    def delivery(self, hold=None):
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR REPLACE INTO deliveries (event_id, relationship_id, kind,"
                "  recipient_task_id, recipient_thread_id, state, attempt_count, hold_reason,"
                "  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                ("event-r", self.relationship, "completion_event", "01parent-task",
                 "01parent-task", "queued", 6, hold, self.clock.iso(), self.clock.iso()))

    def attempt(self, index, state="withheld_pre_send"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO attempts (request_id, event_id, attempt_no, kind,"
                "  internal_state, state, sent_at, observed_at) VALUES (?,?,?,?,?,?,?,?)",
                (f"del-{index:03d}", "event-r", index + 1, "completion_event", "settled",
                 state, self.clock.now(), self.clock.iso()))

    def test_reaching_the_cap_escalates_one_fault_rather_than_forking_a_second(self):
        """The hold reason is mutable, so it cannot be part of identity."""
        self.delivery(hold=None)
        for index in range(3):
            self.attempt(index)
        before = {entry["signature"]["recipient"]: faults.canonical_signature(
            entry["signature"]) for entry in faultsweep.retry_faults(
                self.store, product=PRODUCT, scope={})["observations"]}
        self.delivery(hold="attempt_cap")
        after = {faults.canonical_signature(entry["signature"])
                 for entry in faultsweep.retry_faults(
                     self.store, product=PRODUCT, scope={})["observations"]}
        self.assertEqual(set(before.values()), after)
        for entry in faultsweep.retry_faults(
                self.store, product=PRODUCT, scope={})["observations"]:
            self.ledger.record(entry)
        self.assertEqual(1, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_ledger")["n"])
        self.assertEqual(1, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_publications WHERE kind = ?",
            (faults.OPEN_RECORD,))["n"])

    def test_a_fault_whose_evidence_sits_further_back_is_not_withdrawn_and_reopened(self):
        self.delivery()
        self.attempt(0, state="withheld_pre_send")
        self.attempt(1, state="held_uncertain")
        states = []
        for _round in range(3):
            batch = faultsweep.sweep(self.store)
            faultsweep.record_all(self.ledger, batch, store=self.store)
            states.append(sorted(row["state"] for row in self.store.all(
                "SELECT state FROM fault_ledger")))
        self.assertEqual(states[1], states[2], "the ledger settled")
        self.assertNotIn(faults.WITHDRAWN, states[2])

    def test_a_check_that_fails_after_a_pass_is_not_dropped_as_a_duplicate(self):
        answer = self.ledger.record(omission("a"))
        identifier = answer["faultId"]
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.complete(job, project_ref="proj-CRW", claim_token=claim["claimToken"],
                             readback=operation["block"], external_ref="REL-77")
        self.ledger.record_fix(identifier, ref="PR #1")
        self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                          outcome=faults.FAILED_CHECK)
        self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        last = self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                                 outcome=faults.FAILED_CHECK)
        self.assertTrue(last["recorded"], "the newest failure is its own execution")
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.resolve(identifier)
        self.assertEqual("fault_unverified", refusal.exception.reason.value)

    def test_one_check_repeated_with_nothing_in_between_is_one_execution(self):
        answer = self.ledger.record(omission("a"))
        self.ledger.record_fix(answer["faultId"], ref="PR #1")
        first = self.ledger.record_reverification(answer["faultId"], method="suite",
                                                  ref="pytest", outcome=faults.PASSED)
        again = self.ledger.record_reverification(answer["faultId"], method="suite",
                                                  ref="pytest", outcome=faults.PASSED)
        self.assertFalse(again["recorded"])
        self.assertEqual(first["remediationId"], again["remediationId"])

    def test_a_fault_resolved_before_it_was_ever_filed_still_files_when_it_returns(self):
        first = self.ledger.record(stall("delivery:1"))
        identifier = first["faultId"]
        self.assertEqual(faults.OBSERVED, first["state"])
        self.ledger.record_fix(identifier, ref="PR #1")
        self.ledger.record_reverification(identifier, method="suite", ref="pytest",
                                          outcome=faults.PASSED)
        self.ledger.resolve(identifier)
        reopened = self.ledger.record(stall("delivery:2"))
        self.assertEqual(faults.OPEN, reopened["state"])
        self.assertIsNone(reopened["publication"], "one occurrence is under the threshold")
        queued = self.ledger.record(stall("delivery:3"))
        self.assertEqual(faults.OPEN_RECORD, queued["publication"]["kind"])
        self.assertTrue(queued["publication"]["queued"])
        self.assertEqual([queued["publication"]["publicationId"]],
                         [job["publication_id"] for job in self.ledger.next()])
        self.assertEqual(1, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_publications")["n"])

    def test_a_write_released_by_an_attested_absence_picks_up_the_current_tracker(self):
        self.ledger.set_target(product="crw", project="NEW", team="team-new", project_ref="proj-NEW")
        answer = self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.fail(job, claim_token=claim["claimToken"], error="lost")
        self.ledger.record(dict(omission("b"),
                                scope={"projectKey": "NEW", "issueKey": "NEW-1"}))
        self.ledger.reconcile(job, "nothing here", searched=True, prior_ended=True,
                              reason="the connector answered 400")
        self.assertEqual("team-new", self.store.one(
            "SELECT tracker_ref FROM fault_publications WHERE publication_id = ?",
            (job,))["tracker_ref"])

    def test_a_comment_is_confirmed_only_against_the_issue_its_fault_owns(self):
        answer = self.ledger.record(omission("a"))
        identifier = answer["faultId"]
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        self.ledger.complete(job, project_ref="proj-CRW", claim_token=claim["claimToken"],
                             readback=operation["block"], external_ref="REL-77")
        fix = self.ledger.record_fix(identifier, ref="PR #1")
        comment = fix["publication"]["publicationId"]
        claim = self.ledger.claim(comment, owner="operator")
        operation = self.ledger.operation(comment, claim_token=claim["claimToken"])
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.complete(comment, project_ref="proj-CRW", claim_token=claim["claimToken"],
                                 readback=operation["block"], external_ref="REL-99")
        self.assertEqual("fault_readback_mismatch", refusal.exception.reason.value)
        done = self.ledger.complete(comment, project_ref="proj-CRW", claim_token=claim["claimToken"],
                                    readback=operation["block"])
        self.assertTrue(done["confirmed"])
        self.assertEqual("REL-77", done["external_ref"])

    def test_a_rotation_over_many_full_pages_reaches_the_last_attempt_and_then_wraps(self):
        """A cursor that wrapped after a fixed run of full pages never read what lay past it."""
        self.delivery()
        total = faultsweep.SWEEP_LIMIT * 5 + 3
        for index in range(total):
            self.attempt(index)
        seen, positions = set(), []
        for _round in range(7):
            batch = faultsweep.sweep(self.store)
            faultsweep.record_all(self.ledger, batch, store=self.store)
            seen.update(entry["occurrenceKey"] for entry in batch["observations"]
                        if entry["faultClass"] == "delivery_stalled")
            positions.append(batch["cursors"].get("delivery_retrying"))
        self.assertEqual({f"delivery:del-{index:03d}" for index in range(total)}, seen)
        self.assertIsNone(positions[5], "the short sixth page ends the rotation")
        self.assertIsNotNone(positions[6], "and the next sweep starts another")


class ProvisionalRowsAndBounds(RelayTestCase):
    """PR126 threads kvpuT and kvpv9, each closed as a class.

    The first: an attempt row is inserted in_flight with a provisional held_uncertain state
    before the transport call returns, and every reader of attempt state has to wait for it to
    settle. The second: a bound an operator passes has to reach the query it is meant to bound.
    """

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target(product="crw", project="CRW", team=TRACKER, project_ref="proj-CRW")
        self.register()
        self.relationship = self.store.one(
            "SELECT relationship_id FROM relationships")["relationship_id"]

    def delivery(self, event_id="event-f", hold=None):
        with self.store.transaction() as db:
            db.execute(
                "INSERT OR REPLACE INTO deliveries (event_id, relationship_id, kind,"
                "  recipient_task_id, recipient_thread_id, state, attempt_count, hold_reason,"
                "  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (event_id, self.relationship, "completion_event", "01parent-task",
                 "01parent-task", "queued", 3, hold, self.clock.iso(), self.clock.iso()))

    def attempt(self, index, *, internal, state="held_uncertain", event_id="event-f"):
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO attempts (request_id, event_id, attempt_no, kind,"
                "  internal_state, state, sent_at, observed_at) VALUES (?,?,?,?,?,?,?,?)",
                (f"del-f-{index:03d}", event_id, index + 1, "completion_event", internal,
                 state, self.clock.iso(), self.clock.iso()))

    def test_sends_still_in_flight_are_not_failures(self):
        """Three healthy sends observed mid-flight used to reach the degraded threshold."""
        self.delivery()
        for index in range(3):
            self.attempt(index, internal="in_flight")
        derived = faultsweep.retry_faults(self.store, product=PRODUCT, scope={})
        self.assertEqual([], derived["observations"])
        for _round in range(3):
            faultsweep.record_all(self.ledger, faultsweep.sweep(self.store), store=self.store)
        self.assertEqual(0, self.store.one(
            "SELECT COUNT(*) AS n FROM fault_ledger WHERE fault_class = ?",
            ("delivery_stalled",))["n"])

    def test_a_reconciled_uncertain_outcome_stays_eligible(self):
        """Settled as uncertain means nobody could establish that it landed, which is a fact."""
        self.delivery()
        self.attempt(0, internal="settled", state="held_uncertain")
        self.attempt(1, internal="in_flight")
        derived = faultsweep.retry_faults(
            self.store, product=PRODUCT, scope={})["observations"]
        self.assertEqual(["delivery:del-f-000"], [entry["occurrenceKey"] for entry in derived])

    def test_a_held_delivery_is_named_by_its_last_settled_attempt(self):
        self.delivery(hold="attempt_cap")
        self.attempt(0, internal="settled", state="withheld_pre_send")
        self.attempt(1, internal="in_flight")
        derived = faultsweep.delivery_faults(
            self.store, product=PRODUCT, scope={})["observations"]
        self.assertEqual(1, len(derived))
        self.assertEqual("withheld_pre_send", derived[0]["signature"]["attemptState"])
        self.assertEqual("delivery:del-f-000", derived[0]["occurrenceKey"])

    def test_an_in_flight_row_does_not_keep_a_recovered_fault_open(self):
        signature = {"recipient": "01parent-task", "attemptState": "held_uncertain"}
        self.delivery()
        self.attempt(0, internal="in_flight")
        self.assertIsNone(faultsweep.still_present(self.store, "delivery_stalled", signature))
        self.attempt(1, internal="settled")
        self.assertIsNotNone(faultsweep.still_present(self.store, "delivery_stalled",
                                                      signature))

    def fill(self, count):
        for index in range(count):
            self.ledger.record(faults.observation(
                product=PRODUCT, fault_class="report_omitted", severity=faults.BROKEN,
                signature={"relationship": f"rel-{index:03d}", "turn": "t"},
                occurrence_key=f"k{index}", scope=SCOPE))

    def test_the_listing_is_paged_and_its_cursor_is_stable(self):
        self.fill(5)
        first = self.ledger.snapshot(limit=2)
        self.assertEqual(2, len(first["faults"]))
        self.assertIsNotNone(first["next"])
        # A fault recorded between pages lands after the cursor instead of shifting the pages.
        self.fill(6)
        second = self.ledger.snapshot(limit=2, after=first["next"])
        third = self.ledger.snapshot(limit=10, after=second["next"])
        seen = [row["fault_id"] for page in (first, second, third) for row in page["faults"]]
        self.assertEqual(len(seen), len(set(seen)))
        self.assertEqual(6, len(seen))
        self.assertIsNone(third["next"])

    def test_nested_lists_are_bounded_per_fault(self):
        answer = self.ledger.record(omission("a"))
        for index in range(faults.SHOWN_PER_FAULT + 3):
            self.ledger.record_fix(answer["faultId"], ref=f"PR #{index}")
        row = self.ledger.snapshot(limit=1)["faults"][0]
        self.assertLessEqual(len(row["publications"]), faults.SHOWN_PER_FAULT)
        self.assertTrue(row["publicationsTruncated"])
        self.assertEqual(faults.SHOWN_PER_FAULT,
                         len(self.ledger.remediations(answer["faultId"])))

    def test_a_bound_that_bounds_nothing_is_refused_on_every_listing(self):
        for call in (lambda: self.ledger.snapshot(limit=0),
                     lambda: self.ledger.snapshot(limit=-1),
                     lambda: self.ledger.snapshot(after=-1),
                     lambda: self.ledger.remediations("x", limit=0)):
            with self.assertRaises(faults.FaultRefused):
                call()

    def test_a_sweep_bound_that_bounds_nothing_is_refused(self):
        """The sweep and each public adapter pass limit to SQL, where -1 means unlimited."""
        for call in (
            lambda: faultsweep.sweep(self.store, limit=-1),
            lambda: faultsweep.sweep(self.store, limit=0),
            lambda: faultsweep.retry_faults(self.store, product=PRODUCT, scope={}, limit=-1),
            lambda: faultsweep.delivery_faults(self.store, product=PRODUCT, scope={}, limit=0),
            lambda: faultsweep.sync_faults(self.store, product=PRODUCT, scope={}, limit=-5),
            lambda: faultsweep.observation_faults(self.store, product=PRODUCT, scope={},
                                                  limit=0),
            lambda: faultsweep.recovered(self.store, [], product=PRODUCT, scope={}, limit=-1),
        ):
            with self.assertRaises(faults.FaultRefused):
                call()

    def invoke(self, *argv):
        import contextlib
        import io

        from codex_session_relay import cli

        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = cli.main(["--state", str(self.store.path.parent), *argv])
        return code, json.loads(buffer.getvalue())

    def test_the_command_line_listing_takes_its_bound_and_continues(self):
        self.fill(3)
        code, refusal = self.invoke("fault-show", "--limit", "-1")
        self.assertEqual(2, code)
        self.assertEqual("fault_observation_malformed", refusal["reason"])
        code, first = self.invoke("fault-show", "--limit", "2")
        self.assertEqual(0, code)
        self.assertEqual(2, len(first["faults"]))
        code, rest = self.invoke("fault-show", "--limit", "2", "--after", str(first["next"]))
        self.assertEqual(0, code)
        self.assertEqual(1, len(rest["faults"]))
        self.assertIsNone(rest["next"])


if __name__ == "__main__":
    unittest.main()
