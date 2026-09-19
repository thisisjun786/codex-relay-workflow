"""CRW-94: whether a correction's restoration block reaches the child, and what it costs.

A needs-changes verdict is the only correction channel this workflow allows, so the context a
compacted child needs in order to resume rides inside that verdict's own findings. Three
mechanisms could take it back out and none of them left anything behind that said so.

Every case here is written to fail at bd42ef6 FOR THE DEFECT rather than for a name that did
not exist yet. So the assertions are made against behaviour the relay had then - the rendered
bytes, the journal, whether a generation opened - and refusal reasons are compared as the
strings they are stored and reported as, never as enum members the parent commit lacks.
"""

import json
from unittest import mock
from pathlib import Path
from types import SimpleNamespace

import codex_session_relay
from codex_session_relay import cli, cxc, report
from codex_session_relay.errors import RelayError
from codex_session_relay.identity import ack_proof, revision_request_event_id

from .support import CHILD, PARENT, DeliveryTestCase


def _findings(count, *, carries=None, note_size=40):
    """A correction's findings, with one of them optionally declaring the block."""
    out = []
    for number in range(1, count + 1):
        finding = {
            "id": f"c{number:02d}",
            "verdict": "needs_changes",
            "note": f"finding {number}: " + ("x" * note_size),
        }
        if carries == number:
            finding["note"] = (
                f"finding {number}: RESTORATION BLOCK. workflow CXC Loop; issue CRW-94; "
                "emit under the generation this verdict opens; plan and ledger live in the "
                "task's own record"
            )
            finding["restoration"] = True
        out.append(finding)
    return out


def _verdict_schema():
    path = Path(codex_session_relay.__file__).parent / "schema" / "verification-verdict.json"
    return json.loads(path.read_text(encoding="utf-8"))


class RestorationDelivery(DeliveryTestCase):
    def _acknowledged(self):
        # Both directions authorized, because a needs-changes verdict routes a revision back
        # to the child and an unauthorized recipient is refused long before anything here.
        relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id, ack_proof=ack_proof(event_id, turn.turn_id),
            accepted=True, adapter=self.adapter,
        )
        return relationship, event_id

    def _revision_of(self, relationship, event_id, verdict_turn):
        return revision_request_event_id(
            relationship["relationshipId"], event_id, verdict_turn
        )

    def _projections(self, event_id, kind="restoration_projected"):
        return [
            json.loads(row["detail"])
            for row in self.store.all(
                "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq",
                (kind, event_id),
            )
            if row["detail"]
        ]

    def _projection(self, event_id):
        found = self._projections(event_id)
        # Reported rather than raised, so a failure reads as the silence it is describing.
        return found[-1] if found else {"outcome": "nothing was recorded"}

    def _generations(self):
        return self.store.one("SELECT COUNT(*) AS c FROM generations")["c"]

    @staticmethod
    def _rendered_findings(message):
        """The finding lines only.

        Searched by line shape rather than by substring, because the message is full of hex
        identifiers and a bare 'c11' in a revision hash passes an assertIn that was meant to
        be about a finding. A string match is not a behaviour match, and this file exists to
        make that distinction, so it may not rely on the mistake itself.
        """
        return [
            line for line in message.splitlines()
            if line.startswith("  c") and ": needs_changes" in line
        ]

    def _revisions_queued(self):
        return self.store.one(
            "SELECT COUNT(*) AS c FROM deliveries WHERE kind = 'revision_request'"
        )["c"]

    # ------------------------------------------------------- the renderer's cap

    def test_a_correction_says_what_its_finding_cap_removed(self):
        """Twelve findings, ten rendered, and the two that went used to leave no trace.

        The deliverables block in the same renderer has always said this. The findings block
        did not, so a correction that lost its eleventh and twelfth findings read exactly like
        a correction that only ever had ten.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-cap",
            findings=_findings(12),
        )
        message = self.delivery.preview_message(
            self._revision_of(relationship, event_id, "v-cap")
        )
        rendered = self._rendered_findings(message)
        self.assertEqual(len(rendered), 10)
        self.assertTrue(rendered[-1].startswith("  c10:"), rendered[-1])
        self.assertIn("... 2 more", message)

    def test_a_cap_that_takes_the_block_says_so_by_name(self):
        """A count alone cannot be acted on. Which one went is the thing that decides."""
        relationship, event_id = self._acknowledged()
        # Ruled without the block declared, so the correction is queued the way it would have
        # been before, and the RENDERER is what is under test here.
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-name",
            findings=_findings(12),
        )
        revision = self._revision_of(relationship, event_id, "v-name")
        # Mark the twelfth finding in the stored receipt, which is what the renderer reads.
        row = self.store.one("SELECT receipt FROM events WHERE event_id = ?", (revision,))
        receipt = json.loads(row["receipt"])
        receipt["criteria"][11]["restoration"] = True
        self.store.db.execute(
            "UPDATE events SET receipt = ? WHERE event_id = ?",
            (json.dumps(receipt), revision),
        )
        message = self.delivery.preview_message(revision)
        overflow = [line for line in message.splitlines() if line.startswith("  ... ")]
        self.assertEqual(
            overflow,
            ["  ... 2 more, including the restoration block on c12; see"
             f" 'codex-session-relay show --event {revision}'"],
        )

    # --------------------------------------- the decision, before the generation

    def test_a_block_the_message_cannot_carry_refuses_before_a_generation_opens(self):
        """The whole point of deciding early is that there is something to go back to.

        At the parent commit this verdict succeeded: the generation the child was working in
        was superseded, the correction was queued without the block, and no supported channel
        remained to send it. Refusing rolls all of that back before any of it happens.
        """
        relationship, event_id = self._acknowledged()
        generations = self._generations()
        with self.assertRaises(RelayError) as caught:
            self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v-late",
                findings=_findings(12, carries=12),
            )
        self.assertEqual(caught.exception.reason.value, "restoration_undeliverable")
        self.assertIn("c12", caught.exception.detail)
        self.assertEqual(self._generations(), generations, "no generation may have opened")
        self.assertEqual(self._revisions_queued(), 0, "nothing may have been queued")
        self.assertIsNone(
            self.store.one("SELECT event_id FROM verdicts WHERE event_id = ?", (event_id,)),
            "the ruling itself must have rolled back with everything else",
        )

    def test_a_correction_carrying_no_block_records_that_as_a_result(self):
        """Not carried is a finding about the correction, not the absence of one."""
        _relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-none",
            findings=_findings(3),
        )
        self.assertEqual(self._projection(event_id)["outcome"], "not_carried")

    def test_a_carried_block_is_named_in_the_record_and_in_the_bytes(self):
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-ok",
            findings=_findings(4, carries=1),
        )
        projected = self._projection(event_id)
        self.assertEqual(projected["outcome"], "carried")
        self.assertEqual(projected["criterion"], "c01")
        message = self.delivery.preview_message(
            self._revision_of(relationship, event_id, "v-ok")
        )
        rendered = self._rendered_findings(message)
        self.assertTrue(
            rendered[0].startswith("  c01 [restoration block]:"), rendered[0]
        )

    def test_a_verdict_that_opens_no_correction_says_the_block_had_nowhere_to_go(self):
        """A block attached to a verified ruling travels in nothing, and that is reportable."""
        _relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="verified", verdict_turn_id="v-pass",
            findings=[{"id": "c01", "verdict": "verified"}],
        )
        self.assertEqual(self._projection(event_id)["outcome"], "not_carried")

    def test_a_replayed_ruling_reports_what_it_recorded_rather_than_measuring_again(self):
        _relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-first",
            findings=_findings(4, carries=1),
        )
        replay = self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-second",
            findings=_findings(4),
        )
        self.assertTrue(replay.get("_replay"))
        self.assertEqual(
            len(self._projections(event_id)), 1,
            "a replay returns the ruling of record; it does not measure a second time",
        )
        self.assertEqual(self.ack.restoration_of(event_id)["outcome"], "carried")

    # ------------------------------------------- the budget, told from the cap

    def test_a_report_that_would_push_the_block_out_is_refused_naming_the_budget(self):
        """The budget and the cap are different mechanisms and must not report as one.

        Recording a work report is what moves this event from the legacy renderer to the
        composer, and the composer has a byte budget the legacy renderer does not. A block
        that passed the verdict-time projection can still be squeezed out here - and here is
        the last moment at which saying so costs nothing, because nothing has been sent.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-budget",
            findings=_findings(20, carries=10, note_size=900),
        )
        revision = self._revision_of(relationship, event_id, "v-budget")
        with self.assertRaises(RelayError) as caught:
            report.record(
                self.store, self.clock, event_id=revision,
                repository="thisisjun786/codex-relay-workflow",
                cxc_status=cxc.BLOCKED,
                cxc_reason="the correction is larger than one message can hold",
                summary="twenty findings, one of which carries the restoration block",
                next_action="answer every finding above",
            )
        self.assertEqual(caught.exception.reason.value, "restoration_undeliverable")
        self.assertIn("c10", caught.exception.detail)
        self.assertIn("budget", caught.exception.detail)
        self.assertEqual(
            self._projections(revision, kind="restoration_rendered"), [],
            "a refused report records nothing, because nothing was recorded",
        )

    def test_an_ordinary_report_records_what_became_of_the_block(self):
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-fits",
            findings=_findings(4, carries=1),
        )
        revision = self._revision_of(relationship, event_id, "v-fits")
        recorded = report.record(
            self.store, self.clock, event_id=revision,
            repository="thisisjun786/codex-relay-workflow",
            cxc_status=cxc.BLOCKED, cxc_reason="changes are required on this head",
            summary="four findings, the first of which carries the restoration block",
            next_action="answer every finding above",
            restore={"mode": "CXC Loop", "phase": "B"},
        )
        # Read with a default so a build that records nothing fails as the silence it is,
        # rather than as a KeyError that says nothing about what went wrong.
        reported = recorded.get("restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "carried")
        self.assertEqual(reported.get("restoreSection"), "carried")
        self.assertEqual(
            self._projections(revision, kind="restoration_rendered")[-1]["outcome"], "carried"
        )

    def test_a_report_that_cannot_be_composed_is_refused_rather_than_called_unmeasured(self):
        """A composition failure recurs inside every delivery claim, so it is not unmeasured.

        Committing the report would leave the correction queued behind a message nobody can
        render: each attempt raises the same way and rolls its own claim back. The block is
        stranded either way, which is the same silence arriving by a longer route.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-nofit",
            findings=_findings(4, carries=1),
        )
        revision = self._revision_of(relationship, event_id, "v-nofit")

        # The budget is squeezed below what the message's REQUIRED parts occupy, which is the
        # one way the composer refuses outright instead of shortening. Patched rather than
        # stubbed, and patched on a name both this build and the parent commit have, so the
        # parent fails this case by recording the report rather than by lacking a symbol.
        with mock.patch.object(report, "BUDGET", 400):
            with self.assertRaises(RelayError) as caught:
                report.record(
                    self.store, self.clock, event_id=revision,
                    repository="thisisjun786/codex-relay-workflow",
                    cxc_status=cxc.BLOCKED, cxc_reason="changes are required on this head",
                    summary="a report that cannot be rendered into a message",
                    next_action="answer every finding above",
                )
        self.assertEqual(caught.exception.reason.value, "restoration_undeliverable")
        self.assertIsNone(
            report.read(self.store, revision), "a refused report stores nothing",
        )

    def test_the_report_projection_names_the_attempt_it_measured(self):
        """A projection is preflight, and which attempt it measures decides its own bytes.

        The request id sits on a line of the message, so measuring a1 while the next send
        renders a5 compares a different length. Deferred attempts are ordinary here.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-attempt",
            findings=_findings(4, carries=1),
        )
        revision = self._revision_of(relationship, event_id, "v-attempt")
        self.store.db.execute(
            "UPDATE deliveries SET attempt_count = 4 WHERE event_id = ?", (revision,)
        )
        report.record(
            self.store, self.clock, event_id=revision,
            repository="thisisjun786/codex-relay-workflow",
            cxc_status=cxc.BLOCKED, cxc_reason="changes are required on this head",
            summary="four findings, the first of which carries the restoration block",
            next_action="answer every finding above",
        )
        entries = self._projections(revision, kind="restoration_rendered")
        self.assertEqual([entry.get("attempt") for entry in entries], [5])

    def test_the_projection_is_measured_inside_the_write_lock(self):
        """The attempt this sizes itself against can move before the lock is held.

        A delivery that claims and settles a retry-safe attempt between a preflight read and
        this commit leaves the projection describing an attempt already consumed. At a
        request-id digit boundary that is the difference between recording a block as carried
        and the real message dropping it.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-lock",
            findings=_findings(4, carries=1),
        )
        revision = self._revision_of(relationship, event_id, "v-lock")
        original = report._assert_resubmission

        def settle_an_attempt_first(db, identifier, submission_no):
            # Stands in for a delivery claiming an attempt just before this writer got the
            # lock. A projection measured before the transaction cannot see it.
            db.execute(
                "UPDATE deliveries SET attempt_count = 8 WHERE event_id = ?", (revision,)
            )
            return original(db, identifier, submission_no)

        with mock.patch.object(report, "_assert_resubmission", settle_an_attempt_first):
            report.record(
                self.store, self.clock, event_id=revision,
                repository="thisisjun786/codex-relay-workflow",
                cxc_status=cxc.BLOCKED, cxc_reason="changes are required on this head",
                summary="four findings, the first of which carries the restoration block",
                next_action="answer every finding above",
            )
        entries = self._projections(revision, kind="restoration_rendered")
        self.assertEqual([entry.get("attempt") for entry in entries], [9])

    def test_the_command_reports_the_outcome_without_breaking_the_frozen_record(self):
        """The verdict record is closed, so the annotation may not become one of its fields.

        _replay is already returned this way, and both the conformance suite and the ack tests
        strip underscore-prefixed keys before validating, so that prefix is this package's
        existing mark for a relay-owned annotation on a contract-shaped record.
        """
        _relationship, event_id = self._acknowledged()
        payload = cli.cmd_verdict(
            SimpleNamespace(ack=self.ack),
            SimpleNamespace(
                event=event_id, verdict="needs_changes", verdict_turn="v-cli",
                criterion=None, finding=["c01=needs_changes:resume context"],
                criteria=None, restoration="c01", reason=None,
                expect_criteria_digest=None,
            ),
        )
        reported = payload.get("_restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "carried")
        allowed = set(_verdict_schema()["properties"])
        self.assertEqual(
            {key for key in payload if not key.startswith("_")} - allowed, set(),
            "the contract-shaped half of this output may not gain a property the frozen "
            "record forbids",
        )

    def test_the_carrier_is_matched_the_way_the_findings_are_normalised(self):
        """A finding the verdict accepts must be nameable as the carrier.

        The command splits --finding on '=' and stores whatever precedes it, so
        ' c2=needs_changes:context' is recorded with the id ' c2'. normalise_findings then
        trims it to 'c2' and the verdict accepts it. A carrier match against the raw value
        answers no to a finding the same verdict says yes to, and refuses the selection for a
        reason the refusal does not contain - which in this feature would surface later as a
        block reported not_carried with a cause that is not the real one.
        """
        _relationship, event_id = self._acknowledged()
        payload = cli.cmd_verdict(
            SimpleNamespace(ack=self.ack),
            SimpleNamespace(
                event=event_id, verdict="needs_changes", verdict_turn="v-pad",
                criterion=None, finding=[" c2 =needs_changes:resume context"],
                criteria=None, restoration="c2", reason=None,
                expect_criteria_digest=None,
            ),
        )
        reported = payload.get("_restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "carried")
        self.assertEqual(reported.get("criterion"), "c2")

    def test_a_padded_restoration_argument_names_the_same_finding(self):
        """The mismatch mirrors: normalising one operand is half a rule."""
        _relationship, event_id = self._acknowledged()
        payload = cli.cmd_verdict(
            SimpleNamespace(ack=self.ack),
            SimpleNamespace(
                event=event_id, verdict="needs_changes", verdict_turn="v-pad-arg",
                criterion=None, finding=["c2=needs_changes:resume context"],
                criteria=None, restoration="  c2  ", reason=None,
                expect_criteria_digest=None,
            ),
        )
        reported = payload.get("_restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "carried")
        self.assertEqual(reported.get("criterion"), "c2")

    def test_an_empty_restoration_argument_is_refused_rather_than_skipped(self):
        """Omitted and empty are different answers.

        argparse leaves the option None when it is absent, so a falsy check let
        `--restoration "$UNSET"` skip carrier validation and marking altogether. The verdict
        then opened the next generation recording not_carried, while every other carrier that
        names nothing is refused before that point. An operator who asked for a carrier and
        got silence is exactly what this path exists to remove.
        """
        _relationship, event_id = self._acknowledged()
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_verdict(
                SimpleNamespace(ack=self.ack),
                SimpleNamespace(
                    event=event_id, verdict="needs_changes", verdict_turn="v-empty-arg",
                    criterion=None, finding=["c2=needs_changes:resume context"],
                    criteria=None, restoration="", reason=None,
                    expect_criteria_digest=None,
                ),
            )
        self.assertIn("cannot be empty", str(caught.exception))
        self.assertIsNone(
            self.store.one("SELECT event_id FROM verdicts WHERE event_id = ?", (event_id,)),
            "no ruling may have been recorded",
        )

    def test_the_option_does_not_overwrite_a_findings_own_disclaimer(self):
        """Marking the entry would settle the argument before anyone could see there was one.

        --criteria carries arbitrary JSON, so a finding can arrive already declaring the
        block false while the option names that same criterion. Writing true over it hides
        the contradiction from normalise_findings, and the verdict opens the next generation
        instead of refusing.
        """
        _relationship, event_id = self._acknowledged()
        with self.assertRaises(cli.SystemExit2) as caught:
            cli.cmd_verdict(
                SimpleNamespace(ack=self.ack),
                SimpleNamespace(
                    event=event_id, verdict="needs_changes", verdict_turn="v-disclaim",
                    criterion=None, finding=None,
                    criteria=json.dumps([{
                        "id": "c1", "verdict": "needs_changes", "note": "resume",
                        "restoration": False,
                    }]),
                    restoration="c1", reason=None, expect_criteria_digest=None,
                ),
            )
        self.assertIn("says so once", str(caught.exception))
        self.assertIsNone(
            self.store.one("SELECT event_id FROM verdicts WHERE event_id = ?", (event_id,)),
            "no ruling may have been recorded",
        )

    def test_the_option_does_not_make_an_invalid_declaration_valid(self):
        """Writing true over a bad value would take the refusal away from the rule that owns it.

        normalise_findings refuses a declaration that is not a boolean. Marking the matched
        entry first replaced the bad value with a good one, so that refusal never fired and an
        invalid declaration was recorded as a carrier.
        """
        _relationship, event_id = self._acknowledged()
        with self.assertRaises(RelayError) as caught:
            cli.cmd_verdict(
                SimpleNamespace(ack=self.ack),
                SimpleNamespace(
                    event=event_id, verdict="needs_changes", verdict_turn="v-badflag",
                    criterion=None, finding=None,
                    criteria=json.dumps([{
                        "id": "c1", "verdict": "needs_changes", "note": "resume",
                        "restoration": "yes",
                    }]),
                    restoration="c1", reason=None, expect_criteria_digest=None,
                ),
            )
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")
        self.assertIn("true or false", caught.exception.detail)

    # ------------------------------------------------- an unlocatable declaration

    def test_each_attempt_records_what_its_own_bytes_carried(self):
        """A projection describes the attempt that was next when it ran. This describes bytes.

        A retry-safe attempt that never sent leaves the following render one request-id digit
        longer, so nothing measured earlier can settle what a later attempt carried. The
        transaction that freezes an attempt's message is the only place that can.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-bytes",
            findings=_findings(12, carries=1),
        )
        revision = self._revision_of(relationship, event_id, "v-bytes")
        self.attempt(revision)
        self.assertEqual(
            [(entry.get("outcome"), entry.get("attempt"))
             for entry in self._projections(revision, kind="restoration_attempted")],
            [("carried", 1)],
        )

    def test_an_attempt_that_drops_the_block_records_that_against_its_own_bytes(self):
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-late-bytes",
            findings=_findings(12),
        )
        revision = self._revision_of(relationship, event_id, "v-late-bytes")
        # Declared after the ruling, which is the only way to reach a send that drops it: the
        # verdict itself refuses this arrangement before it opens a generation.
        row = self.store.one("SELECT receipt FROM events WHERE event_id = ?", (revision,))
        receipt = json.loads(row["receipt"])
        receipt["criteria"][11]["restoration"] = True
        self.store.db.execute(
            "UPDATE events SET receipt = ? WHERE event_id = ?",
            (json.dumps(receipt), revision),
        )
        self.attempt(revision)
        self.assertEqual(
            [(entry.get("outcome"), entry.get("attempt"))
             for entry in self._projections(revision, kind="restoration_attempted")],
            [("truncated", 1)],
        )

    def test_show_returns_the_outcome_measured_against_the_sent_bytes(self):
        """The documented inspection command is where a reader goes for this.

        Returning only the preflight projections while withholding the one measurement about
        bytes that exist would be the most misleading arrangement of the three.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-show",
            findings=_findings(4, carries=1),
        )
        revision = self._revision_of(relationship, event_id, "v-show")
        self.attempt(revision)
        payload = cli.cmd_show(
            SimpleNamespace(intake=self.intake, delivery=self.delivery, store=self.store),
            SimpleNamespace(event=revision, message=False),
        )
        self.assertEqual(
            [(entry.get("kind"), entry.get("outcome"), entry.get("attempt"))
             for entry in payload.get("restoration") or []
             if entry.get("kind") == "restoration_attempted"],
            [("restoration_attempted", "carried", 1)],
        )

    def test_show_on_the_revision_the_child_is_sent_to_reports_the_ruling(self):
        """The correction's own message points the child at the revision event.

        Filing the ruling's finding only under the superseded completion left that lookup
        empty until an attempt or a report existed, and an empty list cannot be told from a
        correction nobody measured.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-child-look",
            findings=_findings(3),
        )
        revision = self._revision_of(relationship, event_id, "v-child-look")
        payload = cli.cmd_show(
            SimpleNamespace(intake=self.intake, delivery=self.delivery, store=self.store),
            SimpleNamespace(event=revision, message=False),
        )
        self.assertEqual(
            [entry.get("outcome") for entry in payload.get("restoration") or []
             if entry.get("kind") == "restoration_projected"],
            ["not_carried"],
        )

    def test_a_report_no_attempt_will_render_is_not_refused(self):
        """A correction that already went out is not made undeliverable by a later report.

        The projection sizes a message for the next attempt. Once there is no next attempt,
        refusing a supported update on the strength of a rendering nobody will build takes
        away a record and protects nothing: the bytes that went are already accounted for
        against the attempt that froze them.
        """
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-sent",
            findings=_findings(20, carries=10, note_size=900),
        )
        revision = self._revision_of(relationship, event_id, "v-sent")
        self.attempt(revision)
        self.assertEqual(
            [entry.get("outcome")
             for entry in self._projections(revision, kind="restoration_attempted")],
            ["carried"],
            "the legacy renderer carried it, which is what the child received",
        )
        # Submission 2, because a pre-contract message has already been delivered for this
        # event and _assert_resubmission requires it. That is the arrangement the review
        # named: a supported later submission, against a delivery nothing will render again.
        recorded = report.record(
            self.store, self.clock, event_id=revision,
            repository="thisisjun786/codex-relay-workflow",
            cxc_status=cxc.BLOCKED,
            cxc_reason="the correction is larger than one message can hold",
            summary="twenty findings, one of which carries the restoration block",
            next_action="answer every finding above",
            submission_no=2,
        )
        reported = recorded.get("restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "unmeasured")
        self.assertIn("no further attempt", reported.get("detail", ""))

    def _oversized_correction(self, turn):
        """A declared block at finding 10, which the cap carries and the composer does not."""
        relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id=turn,
            findings=_findings(20, carries=10, note_size=900),
        )
        return self._revision_of(relationship, event_id, turn)

    def _record_oversized(self, revision, **extra):
        return report.record(
            self.store, self.clock, event_id=revision,
            repository="thisisjun786/codex-relay-workflow",
            cxc_status=cxc.BLOCKED,
            cxc_reason="the correction is larger than one message can hold",
            summary="twenty findings, one of which carries the restoration block",
            next_action="answer every finding above", **extra,
        )

    def _assert_measured_in_state(self, state, turn):
        revision = self._oversized_correction(turn)
        self.store.db.execute(
            "UPDATE deliveries SET state = ? WHERE event_id = ?", (state, revision),
        )
        with self.assertRaises(RelayError) as caught:
            self._record_oversized(revision)
        self.assertEqual(caught.exception.reason.value, "restoration_undeliverable")

    def test_a_delivery_still_sending_is_measured(self):
        """sending is unresolved, not finished.

        The bytes of one attempt are frozen and the transport has not answered. A pre-send
        rejection is retry-safe and reconciliation returns the delivery to a claimable state,
        so a report committed during that interval becomes the input to the retry. Treating
        the interval as terminal would skip the check and let the retry drop the block, after
        the generation has opened and with no channel left to send it again.
        """
        self._assert_measured_in_state("sending", "v-sending")

    def test_a_delivery_held_uncertain_is_measured(self):
        """held_uncertain means reconciliation has not established whether anything arrived."""
        self._assert_measured_in_state("held_uncertain", "v-uncertain")

    def test_a_held_delivery_cannot_claim_again_so_its_report_is_not_refused(self):
        """The claim predicate requires hold_reason IS NULL, whatever the state says.

        A correction parked at its attempt cap keeps a claimable-looking state while being
        unable to produce another message, so refusing its report rejects a supported update
        over bytes nobody will build.
        """
        revision = self._oversized_correction("v-held")
        self.store.db.execute(
            "UPDATE deliveries SET hold_reason = ? WHERE event_id = ?",
            ("presend_attempt_cap", revision),
        )
        recorded = self._record_oversized(revision)
        reported = recorded.get("restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "unmeasured")
        self.assertIn("presend_attempt_cap", reported.get("detail", ""))

    def test_a_superseded_relationship_can_never_claim_again(self):
        """A superseded assignment leaves the row queued and the claim refuses it forever.

        Reading only the state and the hold classifies it as retryable, so an oversized
        historical report is refused over bytes no attempt can ever build.
        """
        revision = self._oversized_correction("v-superseded")
        self.store.db.execute(
            "UPDATE relationships SET superseded_by = 'rel-replacement' WHERE"
            " relationship_id = (SELECT relationship_id FROM deliveries WHERE event_id = ?)",
            (revision,),
        )
        recorded = self._record_oversized(revision)
        reported = recorded.get("restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "unmeasured")
        self.assertIn("superseded", reported.get("detail", ""))

    def test_an_answered_correction_can_never_claim_again(self):
        """The child's answer annotates the outstanding revision and _claim honours it.

        That annotation is written for exactly the deliveries whose state cannot be
        rewritten, so the row sits at sending or held_uncertain with its relationship and
        generation still current. Reading the state alone calls it retryable and refuses a
        valid later report over a message no claim will ever build.
        """
        revision = self._oversized_correction("v-answered")
        self.store.db.execute(
            "UPDATE deliveries SET state = 'held_uncertain' WHERE event_id = ?", (revision,),
        )
        self.store.db.execute(
            "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied)"
            " VALUES (?,?,?,0)",
            (revision, "superseded_revision", self.clock.iso()),
        )
        recorded = self._record_oversized(revision)
        reported = recorded.get("restoration") or {"outcome": "nothing was recorded"}
        self.assertEqual(reported.get("outcome"), "unmeasured")
        self.assertIn("already been answered", reported.get("detail", ""))

    def test_a_paused_assignment_is_still_measured_because_resume_exists(self):
        """Inactive is reversible, and a report recorded during a pause survives it.

        The report stays attached to the queued delivery, so the first claim after the
        assignment comes back renders it. Treating the pause as permanent would accept an
        oversized report while nothing could send it and then send it, without the block,
        the moment it could.
        """
        revision = self._oversized_correction("v-paused")
        self.store.db.execute(
            "UPDATE relationships SET status = 'paused' WHERE relationship_id ="
            " (SELECT relationship_id FROM deliveries WHERE event_id = ?)",
            (revision,),
        )
        with self.assertRaises(RelayError) as caught:
            self._record_oversized(revision)
        self.assertEqual(caught.exception.reason.value, "restoration_undeliverable")

    def test_two_findings_cannot_both_declare_the_block(self):
        """Two candidates is a block nobody can locate, which is the silence again."""
        _relationship, event_id = self._acknowledged()
        findings = _findings(4, carries=1)
        findings[2]["restoration"] = True
        with self.assertRaises(RelayError) as caught:
            self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v-two",
                findings=findings,
            )
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")

    def test_a_declaration_that_is_not_a_boolean_is_refused_rather_than_dropped(self):
        _relationship, event_id = self._acknowledged()
        findings = _findings(2)
        findings[0]["restoration"] = "yes"
        with self.assertRaises(RelayError) as caught:
            self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v-str",
                findings=findings,
            )
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")

    def test_a_carrier_with_no_note_is_refused_because_the_note_is_the_block(self):
        """Declaring a carrier is not carrying anything.

        Coverage is satisfied by any actionable finding, not necessarily the declared one, so
        an empty carrier passes every later check: the renderer labels it, the projection
        reports carried, and the generation opens with the child holding nothing.
        """
        _relationship, event_id = self._acknowledged()
        with self.assertRaises(RelayError) as caught:
            self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v-empty",
                criteria=[{"id": "c01", "verdict": "verified", "restoration": True}],
                findings=[{"id": "c02", "verdict": "needs_changes", "note": "fix the test"}],
            )
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")
        self.assertIn("carries no note", caught.exception.detail)

    def test_a_note_supplied_through_the_other_input_still_counts(self):
        """The check runs after the merge, so a note from either input satisfies it."""
        _relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-merged-note",
            criteria=[{"id": "c01", "verdict": "needs_changes", "restoration": True}],
            findings=[{"id": "c01", "verdict": "needs_changes", "note": "resume context"}],
        )
        self.assertEqual(self._projection(event_id)["outcome"], "carried")
    def test_a_declaration_in_one_input_is_not_erased_by_the_other(self):
        """criteria and findings are merged by id and the last entry wins.

        That is right for a disposition and a note and wrong for the declaration: the entry
        that was only meant to add the note would cancel it, and the correction would go out
        reporting that it carried no block at all.
        """
        _relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-merge",
            criteria=[{"id": "c01", "verdict": "needs_changes", "restoration": True}],
            findings=[{"id": "c01", "verdict": "needs_changes", "note": "resume context"}],
        )
        projected = self._projection(event_id)
        self.assertEqual(projected["outcome"], "carried")
        self.assertEqual(projected["criterion"], "c01")

    def test_declaring_and_disclaiming_the_same_block_is_refused(self):
        """A caller that means to cancel it cannot be told from one that forgot."""
        _relationship, event_id = self._acknowledged()
        with self.assertRaises(RelayError) as caught:
            self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v-contra",
                criteria=[{"id": "c01", "verdict": "needs_changes", "restoration": True}],
                findings=[{"id": "c01", "verdict": "needs_changes", "note": "n",
                           "restoration": False}],
            )
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")

    def test_the_same_contradiction_is_refused_in_either_order(self):
        """Saying two things is the contradiction; which was said first is not part of it.

        A merged entry records only a true, so an explicit false left no trace and the check
        could see one ordering. Refusing one arrangement and accepting the mirror image of it
        is not a rule about the caller's intent.
        """
        _relationship, event_id = self._acknowledged()
        with self.assertRaises(RelayError) as caught:
            self.ack.record_verdict(
                event_id, verdict="needs_changes", verdict_turn_id="v-contra-rev",
                criteria=[{"id": "c01", "verdict": "needs_changes", "restoration": False}],
                findings=[{"id": "c01", "verdict": "needs_changes", "note": "n",
                           "restoration": True}],
            )
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")

    def test_repeating_the_same_declaration_is_not_a_contradiction(self):
        """Two entries agreeing is one statement made twice, which is nothing to refuse."""
        _relationship, event_id = self._acknowledged()
        self.ack.record_verdict(
            event_id, verdict="needs_changes", verdict_turn_id="v-agree",
            criteria=[{"id": "c01", "verdict": "needs_changes", "restoration": True}],
            findings=[{"id": "c01", "verdict": "needs_changes", "note": "resume context",
                       "restoration": True}],
        )
        self.assertEqual(self._projection(event_id)["outcome"], "carried")
