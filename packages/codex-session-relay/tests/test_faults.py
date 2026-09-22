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
        self.ledger.set_target("crw:CRW", TRACKER)

    def publish(self, identifier, publication, *, external_ref="REL-77"):
        """Carry one publication all the way to confirmed, as a connector holder would."""
        claim = self.ledger.claim(publication, owner="operator")
        operation = self.ledger.operation(publication, claim_token=claim["claimToken"])
        return self.ledger.complete(
            publication, claim_token=claim["claimToken"],
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
        self.assertIn(refusal.exception.reason.value,
                      ("fault_recurred_after_verification", "fault_state_conflict"))

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
        done = self.ledger.complete(job, readback="issue body\n" + operation["block"],
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
        attested = self.ledger.reconcile(job, "nothing here", searched=True)
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
            self.ledger.complete(job, claim_token=claim["claimToken"], readback=damaged,
                                 external_ref="REL-1")
        self.assertEqual("fault_readback_mismatch", refusal.exception.reason.value)

    def test_a_fault_with_no_configured_target_waits_instead_of_being_filed(self):
        ledger = faults.FaultLedger(self.store, self.clock)
        answer = ledger.record(faults.observation(
            product=PRODUCT, fault_class="report_omitted", severity=faults.BROKEN,
            signature={"relationship": "rel-9", "turn": "t"}, occurrence_key="k",
            scope={"projectKey": "OTHER"}))
        self.assertTrue(answer["publication"]["awaitingTarget"])
        self.assertEqual([], [job for job in ledger.next()
                              if job["fault_id"] == answer["faultId"]])
        ledger.set_target("crw:OTHER", "team-other")
        self.assertIn(answer["faultId"], [job["fault_id"] for job in ledger.next()])


class Sweep(RelayTestCase):
    """The sweep derives what the store shows, and clears by reading the source again."""

    def setUp(self):
        super().setUp()
        self.ledger = faults.FaultLedger(self.store, self.clock)
        self.ledger.set_target("crw:CRW", TRACKER)

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
        return batch, faultsweep.record_all(self.ledger, batch)

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


class DaemonPass(RelayTestCase):
    def daemon(self, ledger):
        from codex_session_relay.daemon import RelayDaemon

        return RelayDaemon(self.store, self.registry, self.intake, None, None, None, None,
                           clock=self.clock, faults=ledger,
                           fault_scope={"projectKey": "CRW"})

    def test_the_tick_records_what_is_broken_and_goes_quiet_once_it_has(self):
        from codex_session_relay.daemon import TickReport

        ledger = faults.FaultLedger(self.store, self.clock)
        ledger.set_target("crw:CRW", TRACKER)
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
        from codex_session_relay.daemon import TickReport

        report = TickReport()
        report.faultsRecorded = 1
        report.quiet = not (report.observed or report.reconciled or report.delivered
                            or report.deferred or report.acksVerified or report.anchorsBound
                            or report.requeued or report.faultsRecorded)
        self.assertFalse(report.quiet)


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
        code, _ = self.invoke("fault-target", "--scope", "crw:CRW", "--tracker-ref", TRACKER)
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
                              "--readback", "@" + readback, "--external-ref", "REL-5")
        self.assertEqual(0, code)
        self.assertTrue(done["confirmed"])
        code, shown = self.invoke("fault-show", "--fault", recorded["faultId"])
        self.assertEqual("REL-5", shown["external_ref"])

    def test_the_command_line_refuses_to_resolve_an_unverified_fault(self):
        self.invoke("fault-target", "--scope", "crw:CRW", "--tracker-ref", TRACKER)
        _code, recorded = self.invoke("fault-observe", "--observation",
                                   json.dumps(omission("cli-1")))
        self.invoke("fault-fix", "--fault", recorded["faultId"], "--ref", "PR #1")
        code, refusal = self.invoke("fault-resolve", "--fault", recorded["faultId"])
        self.assertEqual(2, code)
        self.assertEqual("fault_unverified", refusal["reason"])


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
        faultsweep.record_all(self.ledger, batch)
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
        self.ledger.set_target("crw:CRW", TRACKER)
        self.ledger.record(omission("a"))
        job = self.ledger.next()[0]["publication_id"]
        claim = self.ledger.claim(job, owner="operator")
        operation = self.ledger.operation(job, claim_token=claim["claimToken"])
        with self.assertRaises(faults.FaultRefused) as refusal:
            self.ledger.complete(job, claim_token=claim["claimToken"],
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
        # The registry's own generation 1 has never been polled at all and is correctly
        # derived; what must NOT appear is generation 9, whose current anchor polls fine and
        # whose only failure belongs to a turn the scheduler has moved on from.
        stalled = faultsweep.observation_faults(self.store, product=PRODUCT, scope={})
        self.assertEqual([], [entry for entry in stalled
                              if entry["signature"]["generation"] == 9])
        self.assertTrue(stalled, "the never-polled anchor is still derived")




if __name__ == "__main__":
    unittest.main()
