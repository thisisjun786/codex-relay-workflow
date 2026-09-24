"""CRW-230: an instruction's purpose decides what it competes with, and a held report is named.

On the installed R3 relay (CRW-124 G1, finding F-G1-2) a supervisor recorded a relayed decision
with the supported `linkage-directive --purpose relayed_decision` beside the project's assignment.
The relay read any two live instructions of different digests as contradictory, so the hierarchy
reported instruction_conflict and every upward report for both projects was refused link_conflict
for about 32 minutes, while supervisor-standing answered that the reports were owed and showed no
gap. These cases replay that order through the real command line and the daemon's own tick.

Three things are held here. An instruction has a place from its recorded purpose: one live
assignment and one live scope correction per scope, answers to one message of one purpose must
agree, everything else stands beside them, and an instruction whose purpose nobody recorded cannot
be placed. An instruction that would compete for a place already held is refused where it is
recorded, naming the one in force and how to replace it, and the refusal is retained. And a report
the hierarchy holds is named in supervisor-standing instead of being dropped by the daemon.
"""

import contextlib
import io
import json
import os

from codex_session_relay import cli, linkage, supervision
from codex_session_relay.errors import RefusalReason
from codex_session_relay.models import Endpoint

from .support import HOST
from .test_supervisor_autosend import DaemonChannelCase
from .test_supervisor_channel import INITIATIVE, PROJECT, SUPERVISOR, _Reader

LINK = linkage.link_id(linkage.EXECUTION, linkage.INITIATIVE, INITIATIVE, linkage.PROJECT, PROJECT)
SUCCESSOR = "01supervisor-successor"


def relay(*argv):
    """The real command line, in process: (exit status, parsed answer)."""
    out = io.StringIO()
    try:
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            code = cli.main([str(one) for one in argv])
    except SystemExit as error:
        return error.code, {}
    return code, json.loads(out.getvalue())


class DirectiveCase(DaemonChannelCase):
    """The channel's project under its initiative, instructed through the real command line."""

    def state(self):
        return os.path.dirname(str(self.store.path))

    def direct(self, digest, purpose=None, *, correlation=None, expect=0, task=SUPERVISOR):
        """What the supervisor runs to record one instruction."""
        argv = ["--state", self.state(), "linkage-directive", "--scope-kind", "project",
                "--scope", PROJECT, "--from-task", task, "--from-scope", INITIATIVE,
                "--link", LINK, "--digest", digest]
        if purpose is not None:
            argv += ["--purpose", purpose]
        if correlation is not None:
            argv += ["--correlation", correlation]
        code, answer = relay(*argv)
        self.assertEqual(code, expect, answer)
        return answer

    def legacy(self, digest):
        """An instruction recorded without a purpose, as an older writer or an operator did."""
        return self.linkage.record_directive(
            scope_kind=linkage.PROJECT, scope_key=PROJECT, from_task_id=SUPERVISOR,
            from_scope_key=INITIATIVE, link_id_value=LINK, digest=digest)

    def settle(self, directive_id):
        code, answer = relay("--state", self.state(), "linkage-settle", "--directive",
                             directive_id, "--disposition", "superseded", "--actor", SUPERVISOR,
                             "--reason", "replaced by a later instruction")
        self.assertEqual(code, 0, answer)
        return answer

    def live(self):
        return [one["directiveId"] for one in self.linkage.directives(linkage.PROJECT, PROJECT)
                if one["disposition"] is None]

    def contested(self):
        return [one["directiveId"]
                for one in self.linkage.contested_directives(linkage.PROJECT, PROJECT)]

    def retained(self):
        return [dict(row) for row in self.store.all(
            "SELECT reason, incumbent, challenger, detail FROM linkage_conflicts"
            " WHERE scope_key = ? ORDER BY id", (PROJECT,))]

    def held(self):
        """The report_held gaps an operator reads in supervisor-standing."""
        code, answer = relay("--state", self.state(), "supervisor-standing", "--project", PROJECT)
        self.assertEqual(code, 0, answer)
        return [gap for gap in answer["gaps"] if gap.get("gap") == "report_held"]


class TheG1OrderReportsUpward(DirectiveCase):
    def test_a_relayed_decision_beside_the_assignment_lets_the_report_go_up(self):
        """F-G1-2 exactly: assignment, a finished child, then Jun's decision relayed by --purpose."""
        self.direct("d-assignment", "project_assignment")
        self.completed()
        self.direct("d-decision", "relayed_decision", correlation="crw124-g1-PA-blocked-1-r2")

        report = self.tick()

        self.assertEqual(self.counts(report), (1, 1), report.notes)
        self.assertEqual(len(self.upward()), 1, "the completion reached the supervisor")
        walk = self.linkage.up(relationship_id=self.rid)
        self.assertEqual([one for one in walk["contention"]
                          if one.get("contention") == "instruction_conflict"], [])
        self.assertEqual(self.contested(), [])
        self.assertEqual(self.held(), [])

    def test_a_scope_correction_beside_both_does_not_need_the_others_settled(self):
        """G1's W2 step: S recorded a correction and had to supersede everything by hand."""
        self.direct("d-assignment", "project_assignment")
        self.direct("d-decision", "relayed_decision", correlation="crw124-g1-PA-blocked-1-r2")
        self.direct("d-correction", "scope_correction")
        self.assertEqual(len(self.live()), 3)
        self.completed()

        report = self.tick()

        self.assertEqual(self.counts(report), (1, 1), report.notes)
        self.assertEqual(self.contested(), [])


class ARealConflictIsRefusedWhereItIsRecorded(DirectiveCase):
    def test_a_second_assignment_is_refused_and_names_the_one_in_force(self):
        first = self.direct("d-one", "project_assignment")

        refused = self.direct("d-two", "project_assignment", expect=2)

        self.assertEqual(refused["reason"], RefusalReason.LINK_CONFLICT.value)
        self.assertIn(first["directiveId"], refused["detail"])
        self.assertIn("linkage-settle", refused["detail"])
        self.assertEqual(self.live(), [first["directiveId"]], "the refused one never became live")
        retained = self.retained()
        self.assertEqual(len(retained), 1)
        self.assertEqual(retained[0]["incumbent"], first["directiveId"])
        # A retry is the same contest, retained once rather than once per attempt.
        self.direct("d-two", "project_assignment", expect=2)
        self.assertEqual(len(self.retained()), 1)

    def test_the_replacement_records_once_the_one_in_force_is_superseded(self):
        first = self.direct("d-one", "project_assignment")
        self.settle(first["directiveId"])

        second = self.direct("d-two", "project_assignment")

        self.assertEqual(self.live(), [second["directiveId"]])
        self.assertEqual(self.contested(), [])

    def test_two_different_answers_to_one_message_are_refused(self):
        first = self.direct("d-yes", "relayed_decision", correlation="msg-1")
        refused = self.direct("d-no", "relayed_decision", correlation="msg-1", expect=2)
        self.assertIn(first["directiveId"], refused["detail"])
        self.assertEqual(self.live(), [first["directiveId"]])

    def test_answers_to_two_messages_stand_together(self):
        self.direct("d-first", "relayed_decision", correlation="msg-1")
        self.direct("d-second", "relayed_decision", correlation="msg-2")
        self.assertEqual(len(self.live()), 2)
        self.assertEqual(self.contested(), [])

    def test_a_second_scope_correction_is_refused_answering_a_message_or_not(self):
        """Two corrections standing together leave the parent no recorded order to apply."""
        first = self.direct("d-narrow", "scope_correction")
        self.direct("d-widen", "scope_correction", expect=2)
        self.direct("d-answer", "scope_correction", correlation="msg-1", expect=2)
        self.assertEqual(self.live(), [first["directiveId"]])

        self.settle(first["directiveId"])
        second = self.direct("d-widen", "scope_correction")
        self.assertEqual(self.live(), [second["directiveId"]])

    def test_a_refusal_names_the_message_the_live_correction_answers(self):
        """The refusal says what the one in force answers, as the docs promise."""
        first = self.direct("d-narrow", "scope_correction", correlation="msg-blocked-7")
        refused = self.direct("d-widen", "scope_correction", expect=2)
        self.assertIn(first["directiveId"], refused["detail"])
        self.assertIn("msg-blocked-7", refused["detail"])

    def test_a_purposed_instruction_beside_one_of_unknown_purpose_is_refused(self):
        older = self.legacy("d-legacy")

        refused = self.direct("d-new", "relayed_decision", correlation="msg-1", expect=2)

        self.assertIn(older["directiveId"], refused["detail"])
        self.assertIn("purpose", refused["detail"])
        self.assertEqual(self.live(), [older["directiveId"]])

    def test_the_same_digest_cannot_take_a_purpose_later_even_once_settled(self):
        """The limit the docs state: one digest on one link revision is one directive id."""
        older = self.legacy("d-same")
        self.direct("d-same", "project_assignment", expect=2)
        self.settle(older["directiveId"])
        self.direct("d-same", "project_assignment", expect=2)

        replacement = self.direct("d-same-with-a-purpose", "project_assignment")

        self.assertEqual(self.live(), [replacement["directiveId"]])

    def test_two_instructions_of_unknown_purpose_are_still_recorded_and_contested(self):
        """The purposeless path keeps its contract: both kept, the contest reported."""
        self.direct("d-one")
        self.direct("d-two")
        self.assertEqual(len(self.live()), 2)
        self.assertEqual(len(self.contested()), 2)

    def hand_over(self):
        self.linkage.handover(
            role=linkage.SUPERVISOR, scope_key=INITIATIVE, expect_task_id=SUPERVISOR,
            endpoint=Endpoint(SUCCESSOR, HOST, cwd="/successor", cxc_session="cxc-successor"),
            acknowledged=[], evidence="the supervisor was replaced", actor="test")

    def test_a_place_keeps_one_live_row_across_a_handover(self):
        """The handover moves the link revision, so the successor's identical re-issue would be a
        new directive id. It is refused as already in force rather than kept as a second copy,
        and a different assignment is refused naming the row the predecessor left live."""
        first = self.direct("d-assignment", "project_assignment")
        self.hand_over()

        restated = self.direct("d-assignment", "project_assignment", task=SUCCESSOR, expect=2)
        other = self.direct("d-other", "project_assignment", task=SUCCESSOR, expect=2)

        for refused in (restated, other):
            self.assertEqual(refused["reason"], RefusalReason.LINK_CONFLICT.value)
            self.assertIn(first["directiveId"], refused["detail"])
            self.assertIn("link revision 1", refused["detail"])
        self.assertIn("already has this same project_assignment", restated["detail"])
        self.assertEqual(self.live(), [first["directiveId"]])

        # In its own name, once the predecessor's row is settled.
        self.settle(first["directiveId"])
        mine = self.direct("d-assignment", "project_assignment", task=SUCCESSOR)
        self.assertEqual(self.live(), [mine["directiveId"]])
        self.assertNotEqual(mine["directiveId"], first["directiveId"])

    def test_a_pair_of_one_digest_an_older_writer_left_holds_no_report(self):
        """Reading: one digest is one instruction, so an existing pair is no contest."""
        first = self.direct("d-assignment", "project_assignment")
        self.hand_over()
        # The row an older writer would have added on the new revision, beside the first.
        self.store.db.execute(
            "INSERT INTO scope_directives (directive_id, scope_kind, scope_key, from_task_id,"
            " from_scope_key, link_id, link_kind, digest, reference, revision, disposition,"
            " decided_by, decided_at, recorded_at)"
            " SELECT ?, scope_kind, scope_key, ?, from_scope_key, link_id, link_kind, digest,"
            " reference, revision + 1, NULL, NULL, NULL, recorded_at"
            " FROM scope_directives WHERE directive_id = ?",
            ("dir-restated-by-an-older-writer", SUCCESSOR, first["directiveId"]))
        self.assertEqual(len(self.live()), 2)
        self.completed()

        self.assertEqual(self.contested(), [])
        self.assertEqual(self.held(), [])
        self.assertEqual(self.counts(self.tick())[0], 1, "the report is staged")


class AHeldReportIsNamedWhereTheOperatorLooks(DirectiveCase):
    def contest(self):
        """A contest the new rule still reports: two instructions nobody gave a purpose."""
        self.legacy("d-first")
        self.legacy("d-second")

    def test_a_held_report_is_a_named_gap_in_supervisor_standing(self):
        owed = self.obligation()
        self.contest()

        held = self.held()

        self.assertEqual(len(held), 1, held)
        self.assertEqual(held[0]["reason"], RefusalReason.LINK_CONFLICT.value)
        self.assertEqual(held[0]["relationId"], self.rid)
        self.assertIn(owed["obligationId"], held[0]["obligationIds"])
        self.assertIn("instruction_conflict", held[0]["detail"])
        # The daemon still holds it; what changed is that it is no longer silent.
        self.assertEqual(self.counts(self.tick()), (0, 0))
        self.assertEqual(self.upward(), [])

    def test_a_settled_hierarchy_names_no_hold(self):
        self.direct("d-assignment", "project_assignment")
        self.obligation()
        self.assertEqual(self.held(), [])

    def test_a_report_already_sent_is_not_called_held(self):
        one, message_id = self.staged()
        self.channel.attempt(message_id, self.adapter)
        self.contest()
        self.assertEqual(self.held(), [])

    def test_a_report_already_recorded_is_not_called_held(self):
        """Recorded under its id, it is suppressed for that reason; the hierarchy holds nothing."""
        owed = self.obligation()
        code, answer = relay("--state", self.state(), "supervisor-report-recorded", "--event",
                             owed["basis"]["eventId"])
        self.assertEqual(code, 0, answer)
        self.contest()
        self.assertEqual(self.held(), [])

    def test_a_queued_report_is_called_held(self):
        one, message_id = self.staged()
        self.assertEqual(self.channel.get(message_id)["state"], "queued")
        self.contest()

        held = self.held()

        self.assertEqual([gap["obligationIds"] for gap in held], [[one["obligationId"]]])

    def test_a_capped_report_is_still_called_held(self):
        """A send-budget hold is a reason not to restage, not a sign the report went out."""
        one, message_id = self.staged()
        self.store.db.execute(
            "UPDATE supervisor_messages SET state = 'withheld_pre_send', hold_reason = ?"
            " WHERE message_id = ?", (self.channel.policy.cap_reason("send"), message_id))
        self.contest()

        held = self.held()

        self.assertEqual([gap["obligationIds"] for gap in held], [[one["obligationId"]]])

    def test_a_project_nobody_supervises_is_a_named_hold_of_its_own(self):
        """Nowhere to send is a hold with its own reason, not a report nobody owes."""
        owed = self.obligation()
        standing = supervision.standing_for(self.store, self.linkage, PROJECT)
        channel = self.build_channel()
        channel.linkage = _Reader({
            "state": "resolved", "readable": True, "contention": [],
            "gaps": [{"gap": "no_supervisor", "scopeKind": "project", "scopeKey": PROJECT}],
            "levels": [{"scopeKind": "project", "scopeKey": PROJECT,
                        "owner": {"taskId": "01parent-task"}, "depth": 1}]})

        held = channel.report_holds(standing)

        self.assertEqual([(gap["gap"], gap["reason"]) for gap in held],
                         [("report_held", RefusalReason.UNREGISTERED_SCOPE.value)])
        self.assertEqual(held[0]["obligationIds"], [owed["obligationId"]])
