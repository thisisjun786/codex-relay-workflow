"""JUN-131: can the recipient act on the message, and do the two vocabularies still agree.

Every case here is written from the recipient's side. The question is never whether a
string appears, but whether somebody holding only this message could do the next thing.
"""

import unittest

from codex_session_relay import cxc, report
from codex_session_relay.errors import RefusalReason
from codex_session_relay.models import Endpoint

from .support import CHILD, HOST, PARENT, DeliveryTestCase


def a_report(**overrides):
    base = {
        "repository": "thisisjun786/codex-relay-workflow",
        "pr_number": 12,
        "pr_url": "https://github.com/thisisjun786/codex-relay-workflow/pull/12",
        "pr_state": "ready",
        "base_ref": "dev",
        "base_sha": "c56576d5be412b5bc352dd93b9eb37ab279a12f6",
        "head_sha": "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
        "criteria_digest": "d1e2f3",
        "cxc_status": cxc.DONE,
        "cxc_reason": "every recorded criterion has fresh proof on this head",
        "summary": "delivery messages now lead with the pull request",
        "next_action": "review the diff and record a verdict",
        "evidence": [{"check": "python3 -m pytest", "exitCode": 0, "detail": "214 passed"}],
        "unresolved": ["the CLI verb lands after PR 8 merges"],
    }
    base.update(overrides)
    return base


class Recording(DeliveryTestCase):
    def recorded(self, **overrides):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        fields = a_report(**overrides)
        stored = report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"], **fields
        )
        return relationship, event_id, stored

    def test_the_message_leads_with_what_the_parent_has_to_decide(self):
        _relationship, event_id, _stored = self.recorded()
        message = self.delivery.render_message(event_id)
        # The result, the pull request and the next action all arrive before the identifiers
        # that used to open the message.
        self.assertLess(message.index("result:"), message.index("eventId:"))
        self.assertLess(
            message.index("pull request:"), message.index("relationshipId:"),
            "a recipient reads the pull request before it reads a relationship id",
        )
        self.assertLess(message.index("next:"), message.index("revisionHash:"))
        self.assertIn("thisisjun786/codex-relay-workflow#12", message)
        self.assertIn("c56576d5be412b5bc352dd93b9eb37ab279a12f6", message)
        self.assertIn("a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", message)
        self.assertIn("python3 -m pytest -> exit 0", message)
        self.assertIn("the CLI verb lands after PR 8 merges", message)
        self.assertIn("review the diff and record a verdict", message)
        # And it still says how to answer, which is the parent's required action.
        self.assertIn("ack-proof", message)

    def test_a_done_report_is_not_allowed_to_read_as_a_verification(self):
        _relationship, event_id, _stored = self.recorded()
        message = self.delivery.render_message(event_id)
        self.assertIn("DONE", message)
        self.assertIn("It is not a verification", message)
        # The relay's own verified disposition is untouched by anything in the report.
        self.assertIsNone(
            self.store.one("SELECT 1 FROM verdicts WHERE event_id = ?", (event_id,))
        )
        for fact in ("cxc_done", "pull_request_opened", "review_pass", "required_checks_green"):
            self.assertTrue(cxc.refuse_promotion(fact))

    def test_an_unknown_cxc_status_is_diagnosed_rather_than_defaulted(self):
        error = self.assertRefused(
            RefusalReason.OUTCOME_INCONSISTENT,
            lambda: cxc.check_status("SHIPPED", "ready_for_review"),
        )
        self.assertIn("SHIPPED", error.detail)
        self.assertIn(cxc.VERSION, error.detail, "it names the contract it was read against")
        self.assertIn("DONE", error.detail, "and the set it does accept")

    def test_a_status_cannot_contradict_the_outcome_the_receipt_asserted(self):
        error = self.assertRefused(
            RefusalReason.OUTCOME_INCONSISTENT,
            lambda: cxc.check_status(cxc.BLOCKED, "ready_for_review"),
        )
        self.assertIn("BLOCKED", error.detail)
        self.assertIn("blocked_needs_input", error.detail)

    def test_the_three_human_decision_statuses_collapse_without_losing_which_one_it_was(self):
        human = (cxc.BLOCKED, cxc.UNSAFE, cxc.NEEDS_HUMAN)
        for status in human:
            self.assertEqual(cxc.COMPATIBLE_OUTCOMES[status], ("blocked_needs_input",))
        self.assertEqual(
            len({cxc.MEANING[status] for status in human}), 3,
            "they share one outcome, so the words have to stay distinguishable",
        )
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        # The stored report keeps the original word and its reason, which is what makes the
        # collapse lossless for a reader.
        stored = report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"],
            **a_report(cxc_status=cxc.DONE)
        )
        self.assertEqual(report.read(self.store, event_id)["cxcReason"], stored["cxcReason"])

    def test_a_report_about_an_older_head_cannot_answer_for_the_current_one(self):
        _relationship, event_id, _stored = self.recorded()
        stored = report.read(self.store, event_id)
        error = self.assertRefused(
            RefusalReason.STALE_MARK_CONTEXT,
            lambda: report.assert_current(
                stored, execution_generation=stored["executionGeneration"],
                head_sha="9999999999999999999999999999999999999999",
            ),
        )
        self.assertIn("re-report against the head under review", error.detail)

    def test_a_report_from_an_earlier_generation_cannot_answer_for_the_current_one(self):
        _relationship, event_id, _stored = self.recorded()
        stored = report.read(self.store, event_id)
        self.assertRefused(
            RefusalReason.STALE_GENERATION,
            lambda: report.assert_current(stored, execution_generation=99),
        )

    def test_a_pull_request_number_never_travels_without_its_repository(self):
        _relationship, event_id, _stored = self.recorded()
        mine = report.read(self.store, event_id)
        other = dict(mine)
        other["repository"] = "someone-else/other-project"
        other["relationshipId"] = "rel-different"
        self.assertEqual(report.pr_ref(mine), "thisisjun786/codex-relay-workflow#12")
        self.assertEqual(report.pr_ref(other), "someone-else/other-project#12")
        self.assertNotEqual(
            report.pr_key(mine), report.pr_key(other),
            "the same number on two projects is two pull requests",
        )
        self.assertIn(report.pr_ref(mine), self.delivery.render_message(event_id))

    def test_a_report_naming_a_pull_request_names_the_commit_it_is_about(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        error = self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT,
            lambda: report.record(
                self.store, self.clock, event_id=event_id,
                relationship_id=relationship["relationshipId"],
                execution_generation=receipt["executionGeneration"],
                revision_hash=receipt["revisionHash"], outcome=receipt["outcome"],
                **a_report(head_sha=None)
            ),
        )
        self.assertIn("a later push silently inherits this report", error.detail)

    def test_a_report_with_nothing_to_act_on_is_refused(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        for field in ("summary", "next_action", "repository"):
            self.assertRefused(
                RefusalReason.MALFORMED_RECEIPT,
                lambda field=field: report.record(
                    self.store, self.clock, event_id=event_id,
                    relationship_id=relationship["relationshipId"],
                    execution_generation=receipt["executionGeneration"],
                    revision_hash=receipt["revisionHash"], outcome=receipt["outcome"],
                    **a_report(**{field: "   "})
                ),
            )


class Elision(DeliveryTestCase):
    def test_more_than_ten_findings_are_never_dropped_in_silence(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        unresolved = [f"finding {n}: something specific that still needs doing" for n in range(24)]
        report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"],
            **a_report(unresolved=unresolved)
        )
        message = self.delivery.render_message(event_id)
        self.assertIn("finding 23", message, "a generous budget keeps all of them")
        stored = report.read(self.store, event_id)
        row = self.delivery.get(event_id)
        tight = report.render_completion(
            row, receipt, "del-x-a1", stored, budget=1400,
        )
        self.assertIn("omitted:", tight, "a shortened message says so")
        self.assertIn("show --event", tight, "and says where to read the rest")
        self.assertIn("unresolved:", tight, "the heading survives")
        self.assertIn("... ", tight, "and the count of what is missing survives with it")
        self.assertIn("next:", tight, "the required next action is never what gets dropped")
        self.assertIn("ack-proof", tight, "nor the instruction for answering")

    def test_an_impossible_budget_refuses_instead_of_shipping_a_gutted_message(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"], **a_report()
        )
        stored = report.read(self.store, event_id)
        row = self.delivery.get(event_id)
        with self.assertRaises(ValueError) as caught:
            report.render_completion(row, receipt, "del-x-a1", stored, budget=120)
        self.assertIn("rather than shipping a message that lost them", str(caught.exception))

    def test_the_preview_is_still_not_evidence_of_what_was_sent(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"], **a_report()
        )
        before = self.delivery.preview_message(event_id)
        record = self.attempt(event_id)
        sent = self.delivery.sent_message(record["requestId"])
        self.assertIn(record["requestId"], sent)
        # Before the first attempt the preview predicts attempt 1 correctly, so the two agree.
        self.assertEqual(before, sent)
        # Afterwards it describes the attempt that has NOT run, which is the whole reason a
        # preview is never quoted as evidence of what was delivered.
        after = self.delivery.preview_message(event_id)
        self.assertNotEqual(after, sent)
        self.assertNotIn(record["requestId"], after)
        self.assertEqual(sent, self.delivery.sent_message(record["requestId"]))


class Legacy(DeliveryTestCase):
    def test_an_event_with_no_report_renders_exactly_what_it_always_did(self):
        _relationship, event_id = self.queued_event()
        message = self.delivery.render_message(event_id)
        receipt = self.intake.get(event_id)
        self.assertIn("verification request", message)
        self.assertIn(receipt["manifest"][0]["sha256"], message)
        self.assertNotIn("pull request:", message)
        self.assertNotIn("relay-report/1", message)
        self.assertIsNone(report.read(self.store, event_id))
        self.assertEqual(report.version_of(None), report.LEGACY)

    def test_the_two_message_versions_are_told_apart_explicitly(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        self.assertEqual(report.version_of(report.read(self.store, event_id)), report.LEGACY)
        report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"], **a_report()
        )
        stored = report.read(self.store, event_id)
        self.assertEqual(report.version_of(stored), report.VERSION)
        self.assertIn("contract: relay-report/1", self.delivery.render_message(event_id))


class Verdicts(unittest.TestCase):
    def test_the_verdict_line_is_the_one_the_reviewer_contract_fixes(self):
        self.assertEqual(cxc.verdict_line(cxc.PASS), "VERDICT: PASS")
        self.assertEqual(cxc.verdict_line(cxc.FAIL), "VERDICT: FAIL")
        self.assertEqual(
            cxc.verdict_line(cxc.GO_WITH_FIXES, 2), "VERDICT: GO-WITH-FIXES (blockers=2)"
        )

    def test_a_go_with_fixes_without_a_blocker_count_is_refused(self):
        for bad in (None, 0, -1, True):
            with self.assertRaises(ValueError):
                cxc.verdict_line(cxc.GO_WITH_FIXES, bad)

    def test_a_blocker_count_cannot_be_attached_to_pass_or_fail(self):
        for kind in (cxc.PASS, cxc.FAIL):
            with self.assertRaises(ValueError):
                cxc.verdict_line(kind, 3)

    def test_prose_that_resembles_a_verdict_does_not_parse_as_one(self):
        self.assertIsNone(cxc.parse_verdict_line("we think this is a PASS"))
        self.assertIsNone(cxc.parse_verdict_line("VERDICT: LOOKS FINE"))
        self.assertIsNone(cxc.parse_verdict_line("VERDICT: GO-WITH-FIXES"))
        self.assertEqual(
            cxc.parse_verdict_line("VERDICT: GO-WITH-FIXES (blockers=4)"),
            {"kind": cxc.GO_WITH_FIXES, "blockers": 4},
        )

    def test_a_progress_notice_may_not_wear_a_verdict(self):
        with self.assertRaises(ValueError):
            cxc.assert_reviewed(False)


class Waiting(unittest.TestCase):
    def test_a_bare_timeout_is_neither_a_failure_nor_permission_to_run_it_again(self):
        result = cxc.classify_wait(timed_out=True)
        self.assertEqual(result["state"], cxc.TIMED_OUT)
        self.assertFalse(result["isFailure"])
        self.assertFalse(result["authorisesRerun"])

    def test_the_five_endings_stay_apart(self):
        self.assertEqual(
            cxc.classify_wait(terminal_error="exit 1")["state"], cxc.CONFIRMED_FAILURE
        )
        self.assertEqual(cxc.classify_wait(input_requested=True)["state"], cxc.INPUT_NEEDED)
        self.assertEqual(
            cxc.classify_wait(advancing_evidence=True, timed_out=True)["state"], cxc.PROGRESS,
            "fresh evidence outranks the clock",
        )
        self.assertEqual(
            cxc.classify_wait(observable=False, timed_out=True)["state"], cxc.UNOBSERVABLE
        )
        self.assertEqual(
            cxc.classify_wait(stagnation_confirmed=True)["state"], cxc.CONFIRMED_FAILURE
        )
        self.assertEqual(cxc.classify_wait()["state"], cxc.SUSPECTED_STAGNATION)

    def test_nothing_this_module_returns_authorises_a_rerun(self):
        for kwargs in ({"timed_out": True}, {"observable": False}, {"input_requested": True},
                       {"terminal_error": "boom"}, {"advancing_evidence": True}, {}):
            self.assertFalse(cxc.classify_wait(**kwargs)["authorisesRerun"])


class Provenance(unittest.TestCase):
    def test_the_mapping_points_at_the_install_rather_than_copying_it(self):
        record = cxc.provenance()
        self.assertEqual(record["version"], "0.2.28+codex.20260914090142")
        rules = {source["rule"] for source in record["sources"]}
        for expected in ("DISPATCH-TASK-01", "REVIEW-OUTPUT-01", "LOOP-WAIT-EVIDENCE-01",
                         "ATTEST-EVIDENCE-01", "REVIEW-SYNTHESIS-01"):
            self.assertIn(expected, rules)
        for source in record["sources"]:
            self.assertEqual(len(source["sha256"]), 64)
            self.assertTrue(source["path"].startswith("skills/"))

    def test_a_changed_install_re_verifies_only_what_it_touched(self):
        touched = cxc.affected_by(["skills/loop/references/waiting.md"])
        self.assertEqual(touched, ("LOOP-WAIT-EVIDENCE-01",))
        self.assertEqual(cxc.affected_by(["skills/nothing/here.md"]), ())


class Directions(DeliveryTestCase):
    """The revision request is an instruction, so it is shaped like one."""

    def _revision(self):
        from codex_session_relay import identity

        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="verdict-1",
            criteria=[{"id": "c-1", "verdict": "needs_changes",
                       "note": "the migration script is missing from the manifest"}],
        )
        row = self.store.one("SELECT * FROM deliveries WHERE kind = 'revision_request'")
        return event_id, row["event_id"]

    def test_a_correction_carries_every_field_the_dispatch_contract_names(self):
        _source, revision_event = self._revision()
        receipt = self.intake.get(revision_event) or {}
        row = self.delivery.get(revision_event)
        stored = report.record(
            self.store, self.clock, event_id=revision_event,
            relationship_id=row["relationship_id"],
            execution_generation=receipt.get("executionGeneration") or 2,
            revision_hash=receipt.get("revisionHash") or "0" * 64,
            outcome="blocked_needs_input",
            **a_report(
                cxc_status=cxc.NEEDS_HUMAN,
                cxc_reason="the parent judged the manifest incomplete",
                summary="add the migration script and re-submit",
                next_action="add the migration script to the manifest and emit generation 2",
                review={
                    "kind": cxc.GO_WITH_FIXES, "blockers": 1,
                    "findings": [{
                        "id": "c-1", "verdict": "needs_changes",
                        "note": "the migration script is missing from the manifest",
                        "anchor": "migrations/004_add_reports.sql",
                    }],
                },
            )
        )
        message = report.render_revision(row, receipt, "del-y-a1", stored)
        for field in cxc.DISPATCH_FIELDS:
            self.assertIn(field + ":", message, f"{field} is missing from the instruction")
        self.assertIn(cxc.DECISION_BOUNDARY + ":", message)
        self.assertIn("VERDICT: GO-WITH-FIXES (blockers=1)", message)
        # What was violated, and how to reproduce it, lead the message.
        self.assertLess(message.index("violated criteria:"), message.index("SCOPE:"))
        self.assertIn("anchor: migrations/004_add_reports.sql", message)
        self.assertIn("preserve:", message)
        # And the asymmetry the contract actually has is preserved.
        self.assertIn("nothing to acknowledge", message)
        self.assertNotIn("--ack-proof", message)
        self.assertIn("emit --relationship", message)


class Isolation(DeliveryTestCase):
    def test_two_parents_holding_the_same_pull_request_number_stay_apart(self):
        relationship, event_id = self.queued_event()
        receipt = self.intake.get(event_id)
        report.record(
            self.store, self.clock, event_id=event_id,
            relationship_id=relationship["relationshipId"],
            execution_generation=receipt["executionGeneration"],
            revision_hash=receipt["revisionHash"], outcome=receipt["outcome"], **a_report()
        )
        other = self.registry.register(
            parent=Endpoint("01other-parent", HOST, cwd="/other"),
            child=Endpoint(CHILD, HOST, cwd=self.root),
            issue_key="REL-2", artifact_roots=[self.root],
            allowed_recipients=["01other-parent"], dispatch_request_id="dispatch-2",
            dispatch_turn_id="turn-dispatch-2",
        )
        mine = report.read(self.store, event_id)
        theirs = dict(mine)
        theirs["relationshipId"] = other["relationshipId"]
        theirs["repository"] = "another-org/another-repo"
        self.assertNotEqual(report.pr_key(mine), report.pr_key(theirs))
        self.assertNotIn("another-org", self.delivery.render_message(event_id))
