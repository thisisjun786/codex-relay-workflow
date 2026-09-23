"""An omitted report, derived from the relay's own store and sent with nobody asking.

CRW-148's reporting duty names a required negative control: a child that deliberately omits its
report. CRW-180's reader diagnoses one, but through the marker, which the relay daemon never
reads; so until now an omission reached the level above only when a caller passed its reading in.

These cases hold the store-side derivation to four conditions:

1. ONE predicate. The marker reading and the store reading hand omitted.classify the same facts
   and get the same answer; neither decides anything itself.
2. No false wakes on legacy admissions. A child whose relay never recorded its declarations in
   the store has nothing derived from it, and its omission stays owed and visible as before.
3. The store record is an addition, written by the same command after the marker fact, and a
   failure to write it is in the answer and the exit status.
4. Through the real daemon tick: an omitted report goes up once after the grace, a declared
   in-progress pause never does, a later admitted turn clears it, a restart converges on one
   logical id, a paused or archived supervisor is not woken and keeps it, and a parent who also
   stages by hand causes no second wake.

Every marker and store record a child makes is made through the real command line, in process.
"""

import contextlib
import io
import json
import os
from pathlib import Path
from unittest import mock

from codex_session_relay import cli, intent, marker, omitted, supervision
from codex_session_relay import supervisorchannel as channel_module
from codex_session_relay.admission import admit_explicitly
from codex_session_relay.daemon import RelayDaemon
from codex_session_relay.receipts import ObservationOutcome
from codex_session_relay.store import resolve_state_dir
from codex_session_relay.supervisorchannel import SUPERSEDED_HOLD, SupervisorChannel
from codex_session_relay.transport import DISPATCHED, WITHHELD_PRE_SEND

from .support import CHILD, DISPATCH_TURN, ISSUE
from .test_guard import LATER, NOW, GuardTestCase
from .test_supervisor_autosend import DaemonChannelCase
from .test_supervisor_channel import PROJECT, SUPERVISOR

DISPATCH = "dispatch-1"
ASSIGNMENT = marker.assignment_id(DISPATCH)


def relay(*argv):
    """The real command line, in process: (exit status, parsed answer).

    A command the parser does not know answers its usage status and an empty answer, so a
    relay without it fails on what a case asserts rather than on the parser's exit.
    """
    out = io.StringIO()
    try:
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            code = cli.main([str(one) for one in argv])
    except SystemExit as error:
        return error.code, {}
    return code, json.loads(out.getvalue())


def stored(answer):
    """The store record a child command answered with, or an empty one when it gave none."""
    return answer.get("storeRecord") or {}


class ChildCommands:
    """What a child runs: its claim and its dispositions, through the real command line."""

    def child(self, command, *extra, expect=0):
        code, answer = relay("--state", self.state_directory(), command,
                             "--marker-root", self.marker_root(), "--workspace",
                             self.workspace_path(), "--assignment", ASSIGNMENT,
                             "--session", CHILD, *extra)
        self.assertEqual(code, expect, answer)
        return answer

    def claim_through_cli(self, **kw):
        return self.child("intent-claim", "--dispatch-request-id", DISPATCH,
                          "--first-turn", DISPATCH_TURN, **kw)

    def declare_through_cli(self, outcome, *, turn=DISPATCH_TURN, **kw):
        return self.child("intent-disposition", "--turn", turn, "--outcome", outcome, **kw)


# ----------------------------------------------------------------- condition 1


class OnePredicateForBothReaders(ChildCommands, GuardTestCase):
    """The same facts through observe() and derive() reach classify() equal, and answer equal."""

    def state_directory(self):
        return str(self.store.path.parent)

    def marker_root(self):
        return self.markers

    def workspace_path(self):
        return self.workspace

    def settle(self, relation, status="completed", turn=DISPATCH_TURN):
        """The relay's own settlement of the turn, as the daemon records it."""
        self.intake.record_observation(
            self.assigned_turn(status, turn=turn),
            ObservationOutcome.ORDINARY_TURN_END if status == "completed" else status,
            relationship_id=relation["relationshipId"])

    def managed_with_store_records(self):
        relation = self.managed()
        claimed = self.claim_through_cli()
        self.assertEqual(stored(claimed).get("state"), "recorded")
        return relation

    def both(self, *, turn=DISPATCH_TURN, grace=0):
        """Each reader's answer for one turn, and the facts each handed the one predicate."""
        selection = resolve_state_dir(str(self.store.path.parent))
        self.assertTrue(callable(getattr(omitted, "classify", None))
                        and callable(getattr(omitted, "derive", None)),
                        "one predicate, and a reader of this store alone that calls it")
        with mock.patch.object(omitted, "classify", wraps=omitted.classify) as spy:
            by_marker = omitted.observe(selection, self.markers, self.workspace, self.assignment,
                                        CHILD, turn, LATER, grace=grace)
            by_store = omitted.derive(self.store, self.rid, state_directory=str(selection.path),
                                      now=LATER, grace=grace, turn=turn)
        self.assertEqual(spy.call_count, 2, "each reader asked the one predicate once")
        marker_facts, store_facts = (call.args[0] for call in spy.call_args_list)
        self.assertEqual(marker_facts, store_facts, "the same facts reached the predicate")
        for field in ("reportingState", "reason", "owed", "owedReason"):
            self.assertEqual(by_marker[field], by_store[field], field)
        return by_marker, by_store

    @property
    def rid(self):
        return self.store.one("SELECT relationship_id FROM relationships")["relationship_id"]

    def verdict(self, reading):
        return (reading["reportingState"], reading["reason"], reading["owed"],
                reading["owedReason"])

    def test_an_omission_is_the_same_omission_and_the_same_message(self):
        relation = self.managed_with_store_records()
        self.evaluate()
        self.settle(relation)
        by_marker, by_store = self.both()
        self.assertEqual(self.verdict(by_store),
                         ("unreported", "terminal_without_report", True, omitted.OWED))
        # And the two readings are one reading of one omission as far as a staged message is
        # concerned, so a parent's reporting-show and the daemon's derivation converge on it.
        self.assertEqual(channel_module._reading_key(by_marker),
                         channel_module._reading_key(by_store))

    def test_a_declared_in_progress_pause_reads_in_progress_in_both(self):
        relation = self.managed_with_store_records()
        self.declare_through_cli("in_progress")
        self.evaluate()
        self.settle(relation)
        _by_marker, by_store = self.both()
        self.assertEqual(self.verdict(by_store)[:3],
                         ("in_progress", "declared_in_progress", False))

    def test_a_receipted_readiness_reads_reported_in_both(self):
        relation = self.managed_with_store_records()
        self.emit_ready(relation)
        self.declare_through_cli("ready_for_review")
        self.evaluate()
        self.settle(relation)
        _by_marker, by_store = self.both()
        self.assertEqual(self.verdict(by_store)[:3],
                         ("reported", "declared_ready_receipted", False))

    def test_readiness_without_a_receipt_is_an_omission_in_both(self):
        relation = self.managed_with_store_records()
        self.declare_through_cli("ready_for_review")
        self.evaluate()
        self.settle(relation)
        _by_marker, by_store = self.both()
        self.assertEqual(by_store["currentObservation"]["label"], "receipt_missing")
        self.assertEqual(self.verdict(by_store)[:3], ("unreported", "terminal_without_report",
                                                      True))

    def test_a_final_receipt_without_a_declaration_is_owed_by_neither(self):
        relation = self.managed_with_store_records()
        self.emit_ready(relation)
        self.evaluate()
        self.settle(relation)
        _by_marker, by_store = self.both()
        self.assertEqual(self.verdict(by_store),
                         ("unreported", "terminal_without_report", False,
                          omitted.TURN_RECEIPTED))

    def test_a_later_admitted_turn_clears_what_is_owed_in_both(self):
        relation = self.managed_with_store_records()
        self.evaluate()
        self.settle(relation)
        admit_explicitly(self.store, self.clock, relation["relationshipId"], 1, "turn-later",
                         actor="the parent's steer")
        _by_marker, by_store = self.both()
        self.assertEqual(self.verdict(by_store),
                         ("unreported", "terminal_without_report", False,
                          omitted.LATER_TURN_ADMITTED))

    def test_the_grace_holds_both_and_then_releases_both(self):
        relation = self.managed_with_store_records()
        self.evaluate()
        self.settle(relation)
        _by_marker, inside = self.both(grace=10 ** 9)
        self.assertEqual(self.verdict(inside)[2:], (False, omitted.WITHIN_GRACE))
        _by_marker, past = self.both(grace=1)
        self.assertEqual(self.verdict(past)[2:], (True, omitted.OWED))

    def test_a_registration_for_another_workspace_is_refused_by_both_before_classifying(self):
        """Review 2 on 8ffc74c6: the store reader classified what the marker reader refused.

        The identity the registration names - child, binding, issue, working directory,
        dispatch - is one predicate both readers apply before the facts reach classify().
        """
        relation = self.managed_with_store_records()
        self.evaluate()
        self.settle(relation)
        self.store.db.execute("UPDATE relationships SET child_cwd = ?",
                              (str(Path(self.tmp) / "somewhere-else"),))
        selection = resolve_state_dir(str(self.store.path.parent))
        with mock.patch.object(omitted, "classify", wraps=omitted.classify) as spy:
            by_marker = omitted.observe(selection, self.markers, self.workspace, self.assignment,
                                        CHILD, DISPATCH_TURN, LATER)
            by_store = omitted.derive(self.store, self.rid, state_directory=str(selection.path),
                                      now=LATER, grace=0, turn=DISPATCH_TURN)
        for reading in (by_marker, by_store):
            self.assertEqual((reading["reportingState"], reading["reason"], reading["owed"]),
                             ("unmeasured", "registry_identity_mismatch", False))
        self.assertEqual(spy.call_count, 0, "neither reader classified a turn it cannot place")


# ----------------------------------------------------------------- the daemon


class StoreOmissionCase(ChildCommands, DaemonChannelCase):
    """A managed child under the channel's project, with a marker, settled by the daemon."""

    def setUp(self):
        super().setUp()
        self.markers = Path(self.tmp) / "markers"
        self.markers.mkdir()
        intent.declare_intent(self.markers, workspace=self.root, dispatch_request_id=DISPATCH,
                              issue_key=ISSUE, declared_at=NOW, db_path=str(self.store.path))
        intent.bind(self.markers, workspace=self.root, assignment=ASSIGNMENT, session_id=CHILD,
                    task_id=CHILD, at=NOW)
        intent.register_relationship(self.markers, workspace=self.root, assignment=ASSIGNMENT,
                                     relationship_id=self.rid, dispatch_request_id=DISPATCH,
                                     at=NOW, db_path=str(self.store.path))
        # Read defensively so a relay without the store-side derivation fails on what these
        # cases assert rather than in setUp.
        self.grace = getattr(self.channel.policy, "omission_grace_seconds", 300.0)

    def state_directory(self):
        return os.path.dirname(str(self.store.path))

    def marker_root(self):
        return self.markers

    def workspace_path(self):
        return self.root

    def the_turn_ends(self, turn=DISPATCH_TURN):
        """The child's turn ends on the host, and the daemon settles it itself."""
        self.adapter.start_turn(CHILD, turn_id=turn, status="completed")
        report = self.tick()
        self.assertTrue(self.store.one(
            "SELECT 1 FROM assignment_settlements WHERE relationship_id = ? AND turn_id = ?",
            (self.rid, turn)), "the daemon settled the turn")
        return report

    def omissions(self):
        return [one for one in self.messages()
                if one["obligation_kind"] == supervision.UNREPORTED]

    def the_omission(self):
        found = self.omissions()
        self.assertEqual(len(found), 1, "exactly one message for the one omission")
        return found[0]


class AnLLMThatOmitsItsReport(StoreOmissionCase):
    """CRW-148's required negative control, through the daemon: nobody stages or sends."""

    def test_an_omitted_report_is_derived_from_the_store_and_sent_once_after_the_grace(self):
        self.claim_through_cli()
        settled = self.the_turn_ends()
        self.assertEqual(self.counts(settled), (0, 0), "inside the grace nothing is staged")
        self.assertEqual(self.upward(), [])

        after = self.tick(advance=self.grace + 1)
        self.assertEqual(self.counts(after), (1, 1))
        self.assertEqual(len(self.upward()), 1)
        message = self.only_message()
        self.assertEqual((message["obligation_kind"], message["state"]),
                         (supervision.UNREPORTED, DISPATCHED))
        frozen = json.loads(self.store.one(
            "SELECT reading FROM supervisor_messages WHERE message_id = ?",
            (message["message_id"],))["reading"])
        self.assertEqual(frozen["source"], omitted.STORE_SOURCE)
        self.assertEqual(frozen["selectors"]["turn"], DISPATCH_TURN)

        again = self.tick(advance=3600)
        self.assertEqual(self.counts(again), (0, 0))
        self.assertEqual(len(self.upward()), 1, "one omission, one wake")

    def test_a_declared_in_progress_pause_is_never_sent(self):
        self.claim_through_cli()
        self.declare_through_cli("in_progress")
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        self.tick(advance=3600)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])

    def test_a_later_admitted_turn_clears_the_omission_before_it_is_owed(self):
        self.claim_through_cli()
        self.the_turn_ends()
        admit_explicitly(self.store, self.clock, self.rid, 1, "turn-later",
                         actor="the parent steered the child")
        self.tick(advance=self.grace + 1)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])

    def test_a_later_admitted_turn_clears_a_staged_omission_before_transport(self):
        """Staged while the supervisor could not be woken; the work went on before it could."""
        self.claim_through_cli()
        self.adapter.threads[SUPERVISOR].archived = True
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        message = self.the_omission()
        self.assertEqual(message["state"], WITHHELD_PRE_SEND)

        admit_explicitly(self.store, self.clock, self.rid, 1, "turn-later",
                         actor="the parent steered the child")
        self.adapter.threads[SUPERVISOR].archived = False
        self.tick(advance=self.channel.policy.lifecycle_recheck_seconds + 1)
        self.tick(advance=3600)
        self.assertEqual(self.upward(), [], "I-247: the transport start re-derived it")
        self.assertEqual(self.channel.get(message["message_id"])["hold_reason"],
                         SUPERSEDED_HOLD)


class ARegistrationThatIsNotTheClaimedAssignment(StoreOmissionCase):
    """Review 2 on 8ffc74c6: a registration naming another working directory than the claim."""

    def test_the_daemon_sends_nothing_for_a_turn_the_marker_reader_cannot_place(self):
        self.claim_through_cli()
        self.store.db.execute("UPDATE relationships SET child_cwd = ? WHERE relationship_id = ?",
                              (str(Path(self.tmp) / "somewhere-else"), self.rid))
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        self.tick(advance=3600)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])
        _code, derived = relay("--state", self.state_directory(), "reporting-derive",
                               "--relationship", self.rid)
        self.assertEqual((derived.get("reportingState"), derived.get("reason")),
                         ("unmeasured", "registry_identity_mismatch"))


class OneLogicalOmissionAcrossRestarts(StoreOmissionCase):
    def test_a_restarted_daemon_and_a_redelivered_settlement_converge(self):
        self.claim_through_cli()
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        self.assertEqual(len(self.upward()), 1)
        # A new process over the same store, and the host's ending observed again.
        self.daemon = self.build_daemon(SupervisorChannel(
            self.store, self.registry, self.linkage, self.clock,
            settings=lambda task, runtime=None: {"authorized": task}))
        self.intake.record_observation(self.assigned_turn("completed"), "ordinary_turn_end",
                                       relationship_id=self.rid)
        after = self.tick(advance=3600)
        self.assertEqual(self.counts(after), (0, 0))
        self.assertEqual(len(self.omissions()), 1)
        self.assertEqual(len(self.upward()), 1)


class ASupervisorWhoCannotBeWokenKeepsTheOmission(StoreOmissionCase):
    def test_an_archived_supervisor_is_not_woken_and_the_omission_waits(self):
        self.claim_through_cli()
        self.adapter.threads[SUPERVISOR].archived = True
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        self.assertEqual(self.upward(), [])
        message = self.the_omission()
        self.assertEqual((message["state"], message["hold_reason"]), (WITHHELD_PRE_SEND, None))
        _code, standing = relay("--state", self.state_directory(), "supervisor-standing",
                                "--project", PROJECT)
        self.assertIn(message["obligation_id"],
                      [one["obligationId"] for one in standing["standing"]],
                      "the obligation is kept and visible, not dropped")

        self.adapter.threads[SUPERVISOR].archived = False
        self.tick(advance=self.channel.policy.lifecycle_recheck_seconds + 1)
        self.assertEqual(len(self.upward()), 1)
        self.assertEqual(self.channel.get(message["message_id"])["state"], DISPATCHED)


class AParentWhoAlsoStagesTheOmissionByHand(StoreOmissionCase):
    def test_the_parent_staging_first_leaves_one_message_and_one_wake(self):
        self.claim_through_cli()
        self.the_turn_ends()
        self.clock.advance(self.grace + 1)
        code, staged = relay("--state", self.state_directory(), "supervisor-stage",
                             "--project", PROJECT)
        self.assertEqual(code, 0, staged)
        fresh = [each for each in staged.get("staged", []) if each.get("staged")]
        self.assertEqual(len(fresh), 1, "the parent staged the one omission")
        one = fresh[0]
        self.tick()
        self.tick(advance=3600)
        self.assertEqual([m["message_id"] for m in self.omissions()], [one["messageId"]])
        self.assertEqual(len(self.upward()), 1)


# ----------------------------------------------------------------- condition 2


class ALegacyAdmissionIsNeverDerived(StoreOmissionCase):
    """A child whose relay never said it records declarations here: its silence proves nothing."""

    def test_a_child_that_claimed_without_the_store_record_is_never_woken_for(self):
        # Claimed the way every child did before this change: the marker fact alone.
        intent.publish_claim(self.markers, workspace=self.root, assignment=ASSIGNMENT,
                             session_id=CHILD, dispatch_request_id=DISPATCH,
                             first_turn_id=DISPATCH_TURN, at=NOW)
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        self.tick(advance=3600)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])
        _code, derived = relay("--state", self.state_directory(), "reporting-derive",
                               "--relationship", self.rid)
        self.assertEqual((derived.get("reportingState"), derived.get("reason")),
                         ("unmeasured", "declarations_not_recorded"))

        # Still owed and visible exactly as before: a caller's reading raises it and stages it.
        reading = {"schema": "reporting-observation/1", "reportingState": "unreported",
                   "relationshipId": self.rid, "reason": "terminal_without_report",
                   "selectors": {"state": self.channel.state_directory,
                                 "markerRoot": str(self.markers), "workspace": self.root,
                                 "assignment": ASSIGNMENT, "session": CHILD,
                                 "turn": DISPATCH_TURN}}
        staged = self.channel.stage_standing(PROJECT, observations=[reading])
        self.assertEqual([one["staged"] for one in staged["staged"]], [True])


class AParentsReadingIsAProposalToo(StoreOmissionCase):
    """Review 1 on 460d3bae: an omission staged from a parent's reporting-show reading.

    Where the child's relay records its declarations in the store, the store sees what the
    child declared, so every omission - whatever reading it was staged from - is derived again
    from the store under the staging lock and at the transport start (I-247). A reading taken
    before the child declared the turn in progress must not wake anybody.
    """

    def parents_reading(self):
        return {"schema": "reporting-observation/1", "reportingState": "unreported",
                "relationshipId": self.rid, "reason": "terminal_without_report",
                "selectors": {"state": self.channel.state_directory,
                              "markerRoot": str(self.markers), "workspace": self.root,
                              "assignment": ASSIGNMENT, "session": CHILD,
                              "turn": DISPATCH_TURN}}

    def test_a_declaration_after_the_parent_staged_voids_the_send(self):
        self.claim_through_cli()
        self.the_turn_ends()
        staged = self.channel.stage_standing(PROJECT, observations=[self.parents_reading()])
        self.assertEqual([one.get("staged") for one in staged["staged"]], [True])
        message_id = staged["staged"][0]["messageId"]
        self.declare_through_cli("in_progress")
        self.tick(advance=self.grace + 1)
        self.tick(advance=3600)
        self.assertEqual(self.upward(), [], "the store says the turn declared in_progress")
        self.assertEqual(self.channel.get(message_id)["hold_reason"], SUPERSEDED_HOLD)

    def test_a_declaration_before_the_parent_stages_refuses_the_staging(self):
        self.claim_through_cli()
        self.declare_through_cli("in_progress")
        self.the_turn_ends()
        staged = self.channel.stage_standing(PROJECT, observations=[self.parents_reading()])
        self.assertEqual([one.get("staged") for one in staged["staged"]], [])
        self.tick(advance=self.grace + 1)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])


# ----------------------------------------------------------------- condition 3


class TheStoreRecordIsReportedNeverDropped(StoreOmissionCase):
    def test_a_store_that_cannot_be_written_fails_the_command_after_the_marker_write(self):
        broken = Path(self.tmp) / "not-a-store.sqlite3"
        broken.write_text("this is not a database\n" * 64)
        directory = marker.assignment_dir(self.markers, self.root, ASSIGNMENT)
        intent_file = directory / "intent.json"
        declared = json.loads(intent_file.read_text())
        intent_file.write_text(json.dumps({**declared, "dbPath": str(broken)}))

        claimed = self.claim_through_cli(expect=2)
        self.assertEqual(claimed["outcome"], marker.PUBLISHED, "the marker claim was written")
        self.assertEqual(stored(claimed).get("state"), "failed")
        disposed = self.declare_through_cli("in_progress", expect=2)
        self.assertEqual(disposed["published"], marker.PUBLISHED)
        self.assertEqual(stored(disposed).get("state"), "failed")
        self.assertTrue((directory / "dispositions" / CHILD / (DISPATCH_TURN + ".json")).is_file())

    def test_an_intent_naming_no_store_is_reported_and_is_not_a_failure(self):
        directory = marker.assignment_dir(self.markers, self.root, ASSIGNMENT)
        intent_file = directory / "intent.json"
        declared = json.loads(intent_file.read_text())
        declared.pop("dbPath")
        intent_file.write_text(json.dumps(declared))
        claimed = self.claim_through_cli()
        self.assertEqual((stored(claimed).get("state"), stored(claimed).get("reason")),
                         ("not_recorded", "no_store_recorded"))

    def test_an_intent_that_is_not_an_object_is_reported_rather_than_crashing(self):
        """Devin on 460d3bae: a non-object intent raised after the marker write."""
        directory = marker.assignment_dir(self.markers, self.root, ASSIGNMENT)
        (directory / "intent.json").write_text(json.dumps(["not", "an", "intent"]))
        claimed = self.claim_through_cli()
        self.assertEqual(stored(claimed).get("state"), "not_recorded")
        disposed = self.declare_through_cli("in_progress")
        self.assertEqual((stored(disposed).get("state"), stored(disposed).get("reason")),
                         ("not_recorded", "no_store_recorded"))

    def test_reporting_derive_creates_no_store_where_none_exists(self):
        """Devin on 460d3bae: the read-only derivation created and migrated an empty store."""
        elsewhere = Path(self.tmp) / "no-store-here"
        code, derived = relay("--state", elsewhere, "reporting-derive", "--relationship", self.rid)
        self.assertEqual(code, 0, derived)
        self.assertEqual((derived.get("reportingState"), derived.get("reason")),
                         ("unmeasured", "store_absent"))
        self.assertFalse(any(elsewhere.glob("*.sqlite3")) if elsewhere.exists() else False)

    def test_both_records_mirror_what_the_marker_stands_on(self):
        self.claim_through_cli()
        first = self.declare_through_cli("in_progress")
        self.assertEqual(stored(first).get("state"), "recorded")
        # A create-once conflict in the marker keeps the first; so does the store.
        second = self.declare_through_cli("ready_for_review")
        self.assertEqual(second["published"], intent.CONFLICT)
        self.assertEqual(stored(second).get("state"), "unchanged")
        self.assertEqual(self.store.one(
            "SELECT outcome FROM turn_declarations WHERE turn_id = ?",
            (DISPATCH_TURN,))["outcome"], "in_progress")



class AMarkerTheMarkerReaderCannotReadRecordsNoCapability(StoreOmissionCase):
    """Review 3 on 7f14ebd9: the claim recorded the capability from a marker reporting-show
    answers unmeasured about, and the daemon then woke the supervisor for that turn."""

    def directory(self):
        return marker.assignment_dir(self.markers, self.root, ASSIGNMENT)

    def claim_then_let_the_turn_end_unreported(self):
        claimed = self.claim_through_cli()
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        self.tick(advance=3600)
        return claimed

    def test_a_malformed_marker_fact_records_nothing_and_wakes_nobody(self):
        relationship = self.directory() / "relationship.json"
        fact = json.loads(relationship.read_text())
        relationship.write_text(json.dumps({**fact, "executionGeneration": 0}))
        claimed = self.claim_then_let_the_turn_end_unreported()
        self.assertEqual((stored(claimed).get("state"), stored(claimed).get("reason")),
                         ("not_recorded", "marker_malformed"))
        self.assertEqual(self.store.all("SELECT session_id FROM reporting_sessions"), [])
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])

    def test_an_unreadable_marker_fact_records_nothing_and_wakes_nobody(self):
        attempts = self.directory() / "attempts"
        attempts.mkdir(exist_ok=True)
        (attempts / "1.json").write_bytes(b"\xff\xfe not text at all")
        claimed = self.claim_then_let_the_turn_end_unreported()
        self.assertEqual((stored(claimed).get("state"), stored(claimed).get("reason")),
                         ("not_recorded", "marker_unreadable"))
        self.assertEqual(self.store.all("SELECT session_id FROM reporting_sessions"), [])
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])

class ADeclarationRacingTheDaemon(StoreOmissionCase):
    """Review 4 on ebae6a3b: a tick between the marker publication and the store record.

    The disposition was published to the marker and recorded in the store a moment later, and
    a daemon tick in between derived an omission for a turn the child had just declared. The
    command now holds the store's write lock across both, so the other process's staging waits
    for it - here, with a short busy timeout, it gives up - and nothing is sent.
    """

    def a_second_process(self, ready, go, done, outcome):
        """A daemon over its own connection to the same store, ticking when told to."""
        from codex_session_relay.ack import AckService
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.receipts import ReceiptIntake
        from codex_session_relay.reconcile import Reconciler
        from codex_session_relay.registry import Registry
        from codex_session_relay.store import Store

        store = None
        try:
            store = Store(self.store.path)
            store.db.execute("PRAGMA busy_timeout = 300")
            registry = Registry(store, self.clock)
            intake = ReceiptIntake(store, registry, self.clock)
            delivery = DeliveryService(store, registry, intake, self.clock)
            daemon = RelayDaemon(store, registry, intake, delivery,
                                 AckService(store, registry, intake, delivery, self.clock),
                                 Reconciler(store, registry, delivery, self.clock),
                                 self.adapter, clock=self.clock)
            daemon.supervisor_channel = SupervisorChannel(
                store, registry, Linkage(store, self.clock), self.clock,
                settings=lambda task, runtime=None: {"authorized": task})
            ready.set()
            go.wait(60)
            daemon.tick()
        except Exception as error:  # noqa: BLE001 - a locked store is the expected answer
            outcome["error"] = repr(error)
        finally:
            ready.set()
            if store is not None:
                store.close()
            done.set()

    def test_a_tick_between_the_marker_and_the_store_record_wakes_nobody(self):
        import threading

        self.claim_through_cli()
        self.the_turn_ends()
        self.clock.advance(self.grace + 1)
        ready, go, done, outcome = (threading.Event(), threading.Event(), threading.Event(),
                                    {})
        worker = threading.Thread(target=self.a_second_process,
                                  args=(ready, go, done, outcome), daemon=True)
        worker.start()
        self.assertTrue(ready.wait(60), outcome)
        publish = intent.publish_disposition

        def publish_then_let_the_other_process_tick(*args, **kwargs):
            answer = publish(*args, **kwargs)
            go.set()
            done.wait(60)
            return answer

        with mock.patch.object(intent, "publish_disposition",
                               publish_then_let_the_other_process_tick):
            declared = self.declare_through_cli("in_progress")
        worker.join(60)
        self.assertEqual(stored(declared).get("state"), "recorded")
        self.assertEqual(self.upward(), [], "declared in_progress, yet the supervisor was woken")
        self.tick(advance=60)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])

class ARepairedAdmissionIsOrderedByWhenItWasAdmitted(StoreOmissionCase):
    """Devin PRRT_kwDOUcYZMM6lJm4c: a legacy row admitted again keeps its old rowid.

    admission._record_bound repairs a legacy generation_turns row in place, so ordering the
    generation's admissions by rowid put the turn admitted LAST behind one admitted before it,
    and the earlier turn's omission was reported after the work had gone on.
    """

    def test_a_legacy_turn_admitted_after_an_omitted_one_clears_it(self):
        self.claim_through_cli()
        self.store.db.execute(
            "INSERT INTO generation_turns (relationship_id, execution_generation, turn_id,"
            " evidence, actor, detail, admitted_at) VALUES (?,?,?,?,?,?,?)",
            (self.rid, 1, "turn-legacy", "explicit_admission", "a legacy writer", "",
             self.clock.iso()))
        self.clock.advance(1)
        admit_explicitly(self.store, self.clock, self.rid, 1, "turn-omitted",
                         actor="the child")
        self.the_turn_ends("turn-omitted")
        self.clock.advance(1)
        admit_explicitly(self.store, self.clock, self.rid, 1, "turn-legacy",
                         actor="the parent steered the child")
        self.tick(advance=self.grace + 1)
        self.tick(advance=3600)
        self.assertEqual(self.omissions(), [])
        self.assertEqual(self.upward(), [])
        derived = omitted.derive(self.store, self.rid,
                                 state_directory=self.channel.state_directory,
                                 now=self.clock.iso(), grace=0, turn="turn-omitted")
        self.assertEqual((derived["owed"], derived["owedReason"]),
                         (False, omitted.LATER_TURN_ADMITTED))
