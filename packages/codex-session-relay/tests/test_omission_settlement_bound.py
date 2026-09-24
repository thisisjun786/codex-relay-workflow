"""CRW-238: an omitted turn settles within a bound that neither history nor other assignments move.

CRW-124 K2 (R5 10fd7048, the shared host): a managed child ended its admitted business turn with
no emit and no disposition. The store derives an omission only after the relay settles that turn
itself, and the daemon reached it only through the admitted-turn pager, which walked the whole
generation_turns table - every assignment's rows - one row per visit, visiting four of eighteen
assignments per tick. The turn sat at row 58 and was about 91 minutes from being settled.

These cases build that shape (this assignment among seventeen others, its business turn admitted
as row 58) and hold the daemon to five things:

1. The turn omitted.derive evaluates is found by an index seek and, once its child thread stops
   running, read on the next tick - the one host-wide thread listing a tick makes says which
   children stopped - so it settles within a tick of ending however many rows the store holds
   and however many other children run; its omission then goes up once, after the grace.
2. Without that listing every open frontier turn is still read within ceil(U / F) ticks.
3. A child that reports on that turn is delivered within the same bound.
4. The tick's read budget holds, and the admission pager reads only its own assignment's rows
   and starts a wrapped pass at the first row not yet settled.
5. The readers' own cost does not grow with admission history, and assignment-show,
   reporting-show and reporting-derive name what a derivation is waiting for.
"""

import datetime
import json
import math
from unittest import mock

from codex_session_relay import intent, omitted
from codex_session_relay.admission import admit_explicitly
from codex_session_relay.models import Endpoint, TurnRef
from codex_session_relay.policy import RetryPolicy
from codex_session_relay.receipts import ObservationOutcome
from codex_session_relay.store import resolve_state_dir
from codex_session_relay.transport import DISPATCHED

from .support import CHILD, DISPATCH_TURN, HOST, PARENT, DeliveryTestCase
from .test_daemon import DaemonTestCase
from .test_guard import LATER, GuardTestCase
from .test_supervisor_omission_store import ChildCommands, StoreOmissionCase, relay, stored

BUSINESS = "turn-business"
OTHERS = 17
FOREIGN_ROWS = 57
TICK = 20


def frontier_slice(policy):
    """frontier_reads_per_tick, read defensively so a relay without it fails on the bound."""
    return getattr(policy, "frontier_reads_per_tick", 4)


class ASharedHost(StoreOmissionCase):
    """This assignment among seventeen idle others, fifty-seven settled admissions before its own."""

    def setUp(self):
        super().setUp()
        self.others = [self.other(index) for index in range(OTHERS)]
        for index in range(FOREIGN_ROWS):
            rid, child = self.others[index % OTHERS]
            turn = f"history-{index:02}"
            self.adapter.start_turn(child, turn_id=turn, status="completed")
            admit_explicitly(self.store, self.clock, rid, 1, turn, actor="owner")
            self.settle(rid, child, turn)
        # The standby turn this assignment was dispatched on ended and was settled, as in K2.
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="completed")
        self.settle(self.rid, CHILD, DISPATCH_TURN)
        self.claim_through_cli()

    def settle(self, rid, child, turn):
        self.intake.record_observation(TurnRef(child, turn, "completed"),
                                       ObservationOutcome.ORDINARY_TURN_END, relationship_id=rid)

    def other(self, index, *, running=False):
        """Another assignment on its own child: idle (its anchor settled) unless running."""
        parent, child = f"01parent-other-{index:02}", f"01child-other-{index:02}"
        anchor = f"turn-other-{index:02}"
        for task in (parent, child):
            self.adapter.add_thread(task)
        record = self.registry.register(
            parent=Endpoint(parent, HOST, cwd=f"/p/{index}"),
            child=Endpoint(child, HOST, cwd=f"/c/{index}"),
            issue_key=f"REL-other-{index:02}", artifact_roots=[self.root],
            allowed_recipients=[parent], dispatch_request_id=f"dispatch-other-{index:02}",
            dispatch_turn_id=anchor)
        rid = record["relationshipId"]
        self.adapter.start_turn(child, turn_id=anchor,
                                status="inProgress" if running else "completed")
        if not running:
            self.settle(rid, child, anchor)
        return rid, child

    def business_turn_starts(self, turn=BUSINESS, *, status="inProgress", host=True):
        if host:
            self.adapter.start_turn(CHILD, turn_id=turn, status=status)
        admit_explicitly(self.store, self.clock, self.rid, 1, turn, actor=PARENT,
                         detail="managed business dispatch")

    def business_turn_ends(self, turn=BUSINESS, status="completed"):
        self.adapter.finish_turn(CHILD, turn, status=status)

    def settled(self, turn=BUSINESS):
        return self.store.one(
            "SELECT 1 FROM assignment_settlements WHERE relationship_id = ? AND turn_id = ?",
            (self.rid, turn)) is not None

    def ticks_until_settled(self, limit, turn=BUSINESS):
        for count in range(1, limit + 1):
            self.tick(advance=TICK)
            if self.settled(turn):
                return count
        return None

    def derive(self, turn=None, grace=None):
        return omitted.derive(self.store, self.rid, state_directory=self.state_directory(),
                              now=self.clock.iso(),
                              grace=self.grace if grace is None else grace, turn=turn)

    def shown(self, command, *extra):
        code, answer = relay("--state", self.state_directory(), command,
                             "--relationship", self.rid, *extra)
        self.assertEqual(code, 0, answer)
        return answer

    def this_assignment_last_in_the_rotation(self):
        """Stand the relationship rotation where it reaches this assignment last."""
        order = [one["relationshipId"] for one in self.daemon._active_relationships()]
        cursor = (order.index(self.rid) + 1) % len(order)
        self.store.db.execute(
            "INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at)"
            " VALUES ('scheduler','relationships',?,?) ON CONFLICT(task_id, listing)"
            " DO UPDATE SET cursor = excluded.cursor", (str(cursor), self.clock.iso()))


class AnOmittedTurnAmongManyAssignmentsAndLongHistory(ASharedHost):
    def test_the_k2_shape_is_what_this_case_builds(self):
        self.business_turn_starts()
        row = self.store.one("SELECT rowid AS row FROM generation_turns WHERE turn_id = ?",
                             (BUSINESS,))
        self.assertEqual(row["row"], FOREIGN_ROWS + 1)
        self.assertEqual(len(self.daemon._active_relationships()), OTHERS + 1)

    def test_it_settles_within_one_tick_and_goes_up_once_after_the_grace(self):
        self.business_turn_starts()
        self.tick(advance=TICK)
        self.assertFalse(self.settled(), "a running turn is read, not settled")
        self.business_turn_ends()
        self.tick(advance=TICK)
        self.assertTrue(self.settled(),
                        "the omitted business turn waited behind the admission history")
        reading = self.derive()
        self.assertEqual((reading["reportingState"], reading["owed"], reading["owedReason"]),
                         ("unreported", False, omitted.WITHIN_GRACE))
        self.assertEqual(self.omissions(), [])

        after = self.tick(advance=self.grace + 1)
        self.assertEqual(self.counts(after), (1, 1))
        message = self.the_omission()
        self.assertEqual(message["state"], DISPATCHED)
        again = self.tick(advance=3600)
        self.assertEqual(self.counts(again), (0, 0))
        self.assertEqual(len(self.upward()), 1, "one omission, one wake")

    def test_a_stopped_turn_is_read_first_however_many_children_run(self):
        for index in range(12):
            self.other(OTHERS + index, running=True)
        self.business_turn_starts()
        for _ in range(3):
            self.tick(advance=TICK)
        self.business_turn_ends()
        self.assertEqual(self.ticks_until_settled(1), 1,
                         "twelve running children delayed the one that stopped")

    def test_without_the_listing_every_open_turn_is_read_within_ceil_u_over_f(self):
        running = [self.other(OTHERS + index, running=True) for index in range(12)]
        self.adapter.recent_threads = None
        self.business_turn_starts()
        for _ in range(3):
            self.tick(advance=TICK)
        self.business_turn_ends()
        bound = math.ceil((len(running) + 1) / frontier_slice(self.daemon.policy))
        self.assertIsNotNone(self.ticks_until_settled(bound),
                             f"not settled within ceil(U/F) = {bound} ticks of ending")

    def test_the_listing_pages_to_its_watermark(self):
        """Five running children fill two pages of two; the stopped one is on the third."""
        self.daemon.policy = RetryPolicy(thread_activity_listing_limit=2,
                                         thread_activity_listing_pages=4)
        for index in range(5):
            self.other(OTHERS + index, running=True)
        self.business_turn_starts()
        for _ in range(3):
            self.tick(advance=TICK)
        self.business_turn_ends()
        self.assertEqual(self.ticks_until_settled(1), 1,
                         "the stopped child was below the first page and nobody paged to it")

    def test_a_saturated_listing_is_noted_and_the_floor_applies(self):
        self.daemon.policy = RetryPolicy(thread_activity_listing_limit=2,
                                         thread_activity_listing_pages=2)
        running = [self.other(OTHERS + index, running=True) for index in range(6)]
        self.business_turn_starts()
        for _ in range(3):
            self.tick(advance=TICK)
        self.business_turn_ends()
        report = self.tick(advance=TICK)
        self.assertTrue(any("saturated" in note for note in report.notes), report.notes)
        bound = math.ceil((len(running) + 1) / frontier_slice(self.daemon.policy))
        self.assertTrue(self.settled() or self.ticks_until_settled(bound - 1) is not None,
                        f"not settled within ceil(U/F) = {bound} ticks of ending")

    def test_a_crowded_watermark_second_saturates_the_listing(self):
        """More than L x P threads on the watermark's second: the listing cannot reach below it."""
        self.daemon.policy = RetryPolicy(thread_activity_listing_limit=2,
                                         thread_activity_listing_pages=2)
        self.business_turn_starts()
        for _ in range(3):
            self.tick(advance=TICK)
        # Six parents (ids sorting above this child's) touched in the second the child stops.
        for index in range(6):
            self.adapter.threads[f"01parent-other-{index:02}"].updated_at = self.clock.now()
        self.business_turn_ends()
        report = self.tick(advance=TICK)
        self.assertTrue(any("saturated" in note for note in report.notes), report.notes)
        # One open frontier turn, so the floor reads it in this same tick.
        self.assertTrue(self.settled())

    def test_a_failed_listing_keeps_its_watermark(self):
        """A listing that fails part way must not move the mark past rows it never read."""
        self.daemon.policy = RetryPolicy(thread_activity_listing_limit=2,
                                         thread_activity_listing_pages=4)
        for index in range(5):
            self.other(OTHERS + index, running=True)
        self.business_turn_starts()
        self.tick(advance=TICK)
        before = getattr(self.daemon, "_activity_since", None)
        self.assertIsNotNone(before, "a listing that read its pages sets the mark")
        original = self.adapter.recent_threads

        def failing(limit, cursor=None):
            if cursor is not None:
                raise ConnectionError("the second page never came")
            return original(limit, cursor=cursor)

        self.adapter.recent_threads = failing
        report = self.tick(advance=TICK)
        self.assertTrue(any("thread activity listing failed" in note for note in report.notes),
                        report.notes)
        self.assertEqual(self.daemon._activity_since, before)

    def test_a_failed_listing_is_noted_and_the_floor_still_settles_the_turn(self):
        running = [self.other(OTHERS + index, running=True) for index in range(12)]
        self.adapter.fail_reads("recent_threads")
        self.business_turn_starts()
        report = self.tick(advance=TICK)
        self.assertTrue(any("thread activity listing failed" in note for note in report.notes),
                        report.notes)
        self.business_turn_ends()
        bound = math.ceil((len(running) + 1) / frontier_slice(self.daemon.policy))
        self.assertIsNotNone(self.ticks_until_settled(bound))


class ANormalEmitOnTheBusinessTurn(ASharedHost):
    def test_a_ready_receipt_goes_to_the_parent_within_the_bound(self):
        self.business_turn_starts()
        path = self.artifact("out.txt", "the deliverable")
        payload = self.ready_payload(self.relationship, [path],
                                     turn=TurnRef(CHILD, BUSINESS, "inProgress"))
        self.accept(payload)
        self.assertEqual(self.intake.row(payload["eventId"])["stage"], "staged")
        self.this_assignment_last_in_the_rotation()
        self.business_turn_ends()

        self.tick(advance=TICK)

        self.assertEqual(self.intake.row(payload["eventId"])["stage"], "final",
                         "the reported turn waited for the rotation to come round")
        self.assertTrue([one for one in self.adapter.sends if one[1] == PARENT],
                        "the parent was not told within the bound")
        self.assertFalse(self.derive()["owed"])


class TheTickBudget(ASharedHost):
    def test_frontier_and_rotation_never_exceed_the_budget_or_read_a_turn_twice(self):
        """A guard: it holds on 10fd7048 too, and the frontier must not break it."""
        for index in range(12):
            self.other(OTHERS + index, running=True)
        self.business_turn_starts()
        reads = []
        original = self.adapter.read_turn

        def counted(thread, turn):
            reads.append((thread, turn))
            if turn == "turn-other-20":
                raise ConnectionError("this one never answers")
            return original(thread, turn)

        self.adapter.read_turn = counted
        budget = self.daemon.policy.max_turn_reads_per_tick
        for tick in range(10):
            del reads[:]
            self.tick(advance=TICK)
            self.assertLessEqual(len(reads), budget, f"tick {tick} read {len(reads)}")
            # Distinct children here, so a (thread, turn) is one assignment's turn.
            self.assertEqual(len(reads), len(set(reads)), f"tick {tick} read a turn twice")
            if tick == 4:
                self.business_turn_ends()

    def test_the_listing_is_bounded_a_tick_and_absent_while_nothing_is_open(self):
        calls = []
        original = getattr(self.adapter, "recent_threads", None)

        def listed(limit, cursor=None):
            calls.append((limit, cursor))
            return original(limit, cursor=cursor)

        self.adapter.recent_threads = listed
        for _ in range(3):
            self.tick(advance=TICK)
        self.assertEqual(calls, [], "an idle store asked the host for its threads")
        self.business_turn_starts()
        self.tick(advance=TICK)
        pages = getattr(self.daemon.policy, "thread_activity_listing_pages", 4)
        self.assertTrue(1 <= len(calls) <= pages, f"{len(calls)} listing calls in one tick")


class ABudgetOfOne(DaemonTestCase):
    def test_the_rotation_still_reads_when_the_frontier_has_no_room(self):
        """A guard: a budget of one leaves the frontier no slot and the rotation its old one."""
        from codex_session_relay.daemon import RelayDaemon

        self.register()
        self.daemon = RelayDaemon(self.store, self.registry, self.intake, self.delivery,
                                  self.ack, self.reconciler, self.adapter, clock=self.clock,
                                  policy=RetryPolicy(max_turn_reads_per_tick=1))
        self.adapter.start_turn(CHILD, turn_id=DISPATCH_TURN, status="inProgress")
        reads = []
        original = self.adapter.read_turn
        self.adapter.read_turn = lambda thread, turn: (reads.append(turn),
                                                       original(thread, turn))[1]
        self.daemon.tick(now=self.clock.now())
        self.assertEqual(reads, [DISPATCH_TURN])
        self.adapter.finish_turn(CHILD, DISPATCH_TURN, status="completed")
        for _ in range(2):
            self.clock.advance(TICK)
            self.daemon.tick(now=self.clock.now())
        self.assertIsNotNone(self.store.one(
            "SELECT 1 FROM assignment_settlements WHERE turn_id = ?", (DISPATCH_TURN,)))


class TheScopedPager(ASharedHost):
    def pages(self):
        """Every row the admission pager's own query returned, in order."""
        original = self.store.all
        seen = []

        def observed(sql, params=()):
            rows = original(sql, params)
            if "admission_row" in sql:
                seen.extend(dict(row) for row in rows)
            return rows

        self.store.all = observed
        return seen

    def test_another_assignments_rows_are_never_in_this_ones_page(self):
        self.business_turn_starts()
        seen = self.pages()
        relation = self.registry.get(self.rid)
        first = self.daemon._turns_to_poll(relation, 2)
        for _ in range(3):
            self.daemon._turns_to_poll(relation, 2)
        owners = {self.store.one("SELECT relationship_id FROM generation_turns WHERE rowid = ?",
                                 (row["admission_row"],))["relationship_id"] for row in seen}
        self.assertEqual(owners, {self.rid}, "the page walked other assignments' rows")
        self.assertIn(BUSINESS, first, "the first visit did not reach this assignment's admission")

    def test_a_wrapped_pass_starts_at_the_first_row_not_yet_settled(self):
        earlier = [f"settled-{index:02}" for index in range(40)]
        for turn in earlier:
            self.business_turn_starts(turn, status="completed")
            self.settle(self.rid, CHILD, turn)
        self.business_turn_starts("still-running")
        for index in range(4):
            self.business_turn_starts(f"after-{index}", status="completed")
            self.settle(self.rid, CHILD, f"after-{index}")
        seen = self.pages()
        relation = self.registry.get(self.rid)
        picked = []
        for _ in range(300):
            picked.extend(self.daemon._turns_to_poll(relation, 2))
        counts = {}
        for row in seen:
            counts[row["turn_id"]] = counts.get(row["turn_id"], 0) + 1
        self.assertEqual({turn: counts.get(turn, 0) for turn in earlier
                          if counts.get(turn, 0) != 1}, {},
                         "a settled row before the first unsettled one was read again")
        self.assertGreater(picked.count("still-running"), 1,
                           "the unsettled row stopped being read once passed")

    def test_a_cursor_left_past_this_assignments_rows_starts_again_at_the_floor(self):
        """A cursor written by 10fd7048 keeps global rowids; its range can hold none of ours."""
        for index in range(3):
            self.business_turn_starts(f"settled-{index}", status="completed")
            self.settle(self.rid, CHILD, f"settled-{index}")
        self.business_turn_starts("still-running")
        last = self.store.one("SELECT MAX(rowid) AS m FROM generation_turns")["m"]
        self.store.db.execute(
            "INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at)"
            " VALUES ('scheduler',?,?,?)",
            (f"admitted:{self.rid}", json.dumps({"after": last, "through": last + 5}),
             self.clock.iso()))
        relation = self.registry.get(self.rid)
        picked = []
        for _ in range(4):
            picked.extend(self.daemon._turns_to_poll(relation, 2))
        self.assertIn("still-running", picked)


class TheWaitingReason(ASharedHost):
    def waiting(self, **kw):
        return self.derive(**kw).get("waiting") or {}

    def test_each_stage_of_the_wait_is_named_by_every_reader(self):
        self.business_turn_starts()
        first = self.derive()
        self.assertEqual(first["reason"], "host_terminal_unobserved")
        self.assertEqual(self.waiting(), dict(self.waiting(), **{
            "for": "relay_settlement", "reason": "not_yet_polled"}))

        self.tick(advance=TICK)
        running = self.waiting()
        self.assertEqual((running.get("reason"), running.get("lastStatus")),
                         ("in_progress_at_last_read", "inProgress"))
        self.assertIsNotNone(running.get("lastPolledAt"))
        reporting = self.shown("assignment-show").get("reporting") or {}
        self.assertEqual((reporting.get("waiting") or {}).get("reason"),
                         "in_progress_at_last_read")

        self.business_turn_ends()
        self.tick(advance=TICK)
        settled_at = self.store.one(
            "SELECT settled_at FROM assignment_settlements WHERE relationship_id = ?"
            " AND turn_id = ?", (self.rid, BUSINESS))["settled_at"]
        within = self.waiting()
        self.assertEqual(within.get("for"), "report_grace")
        self.assertEqual(intent.moment(within["until"]) - intent.moment(settled_at),
                         datetime.timedelta(seconds=self.grace))
        self.assertEqual((self.shown("reporting-derive").get("waiting") or {}).get("for"),
                         "report_grace")
        reporting = self.shown("assignment-show").get("reporting") or {}
        self.assertEqual(((reporting.get("waiting") or {}).get("for"), reporting.get("turn")),
                         ("report_grace", BUSINESS))

        self.tick(advance=self.grace + 1)
        self.assertNotIn("waiting", self.derive(), "an owed omission is not waiting")

    def test_an_absent_turn_a_failed_read_and_an_unsettled_ending_are_named(self):
        self.business_turn_starts("turn-absent", host=False)
        self.tick(advance=TICK)
        self.assertEqual(self.waiting().get("reason"), "host_reports_absent")

        self.business_turn_starts("turn-rolled-back", status="completed")
        with mock.patch.object(self.intake, "record_observation_in",
                               side_effect=RuntimeError("the disk is full")):
            report = self.tick(advance=TICK)
        self.assertTrue(any("settlement rolled back" in note for note in report.notes),
                        report.notes)
        self.assertEqual(self.waiting().get("reason"), "terminal_read_unsettled")

        self.business_turn_starts("turn-unreadable")
        self.adapter.fail_reads("read_turn")
        self.tick(advance=TICK)
        failed = self.waiting()
        self.assertEqual(failed.get("reason"), "last_read_failed")
        self.assertIn("unavailable", failed.get("lastError") or "")

    def test_a_reading_of_another_generation_is_not_this_turns_wait(self):
        self.business_turn_starts()
        self.store.db.execute(
            "INSERT INTO poll_observations (relationship_id, execution_generation, turn_id,"
            " last_status, last_polled_at, last_attempt_at, last_error) VALUES (?,?,?,?,?,?,?)",
            (self.rid, 2, BUSINESS, "completed", self.clock.iso(), self.clock.iso(), None))
        self.assertEqual(self.waiting().get("reason"), "not_yet_polled")


class AssignmentShowStillAnswersWhenTheReadingCannot(ASharedHost):
    def test_a_store_error_in_the_reading_is_named_not_raised(self):
        import sqlite3

        self.business_turn_starts()
        with mock.patch.object(omitted, "derive",
                               side_effect=sqlite3.OperationalError("database is locked")):
            answer = self.shown("assignment-show")
        reporting = answer.get("reporting") or {}
        self.assertEqual(reporting.get("reportingState"), "unmeasured")
        self.assertIn("database is locked", reporting.get("reason") or "")


class AssignmentShowByIssue(ASharedHost):
    def test_each_assignment_of_the_issue_carries_its_reading(self):
        from .support import ISSUE

        self.business_turn_starts()
        self.tick(advance=TICK)
        code, answer = relay("--state", self.state_directory(), "assignment-show",
                             "--issue", ISSUE)
        self.assertEqual(code, 0, answer)
        mine = [one for one in answer.get("assignments", [])
                if one.get("relationshipId") == self.rid]
        self.assertEqual(len(mine), 1, answer)
        reporting = mine[0].get("reporting") or {}
        self.assertEqual((reporting.get("waiting") or {}).get("reason"),
                         "in_progress_at_last_read")


class TheMarkerReaderNamesTheWaitToo(ChildCommands, GuardTestCase):
    def state_directory(self):
        return str(self.store.path.parent)

    def marker_root(self):
        return self.markers

    def workspace_path(self):
        return self.workspace

    def test_both_readers_name_the_same_wait(self):
        relation = self.managed()
        self.assertEqual(stored(self.claim_through_cli()).get("state"), "recorded")
        self.evaluate()
        selection = resolve_state_dir(str(self.store.path.parent))
        rid = relation["relationshipId"]

        def both():
            by_marker = omitted.observe(selection, self.markers, self.workspace, self.assignment,
                                        CHILD, DISPATCH_TURN, LATER)
            by_store = omitted.derive(self.store, rid, state_directory=str(selection.path),
                                      now=LATER, grace=0, turn=DISPATCH_TURN)
            for reading in (by_marker, by_store):
                self.assertEqual(reading["reason"], "host_terminal_unobserved")
            self.assertEqual(by_marker.get("waiting"), by_store.get("waiting"))
            return by_marker.get("waiting") or {}

        self.assertEqual(both().get("reason"), "not_yet_polled")
        # What the daemon's _record_poll writes for a read that found the turn running.
        self.store.db.execute(
            "INSERT INTO poll_observations (relationship_id, execution_generation, turn_id,"
            " last_status, last_polled_at, last_attempt_at, last_error) VALUES (?,?,?,?,?,?,?)",
            (rid, 1, DISPATCH_TURN, "inProgress", LATER, LATER, None))
        self.assertEqual(both().get("reason"), "in_progress_at_last_read")


class TheReaderIsBoundedInHistory(ASharedHost):
    def materialized(self, callable_):
        """How many bytes of row values a call read through this store."""
        total = [0]
        one, every = self.store.one, self.store.all

        def count(rows):
            for row in rows:
                total[0] += sum(len(str(value)) for value in tuple(row))
            return rows

        with mock.patch.object(self.store, "one",
                               lambda sql, params=(): (lambda row: count([row])[0] if row
                                                       else row)(one(sql, params))), \
                mock.patch.object(self.store, "all",
                                  lambda sql, params=(): count(every(sql, params))):
            answer = callable_()
        return total[0], answer

    def admissions(self, count, start=0):
        for index in range(start, start + count):
            self.business_turn_starts(f"b-{index:04}", status="completed")
            self.settle(self.rid, CHILD, f"b-{index:04}")

    def test_what_derive_reads_does_not_grow_with_the_generations_admissions(self):
        self.admissions(20)
        short, answer = self.materialized(lambda: self.derive())
        self.assertEqual(answer["selectors"]["turn"], "b-0019")
        self.admissions(200, start=20)
        long, answer = self.materialized(lambda: self.derive())
        self.assertEqual(answer["selectors"]["turn"], "b-0219")
        self.assertLess(long - short, 64, f"{short} bytes with 20 admissions, {long} with 220")

    def test_what_a_tick_reads_does_not_grow_with_the_generations_admissions(self):
        """A guard on the tick as a whole: the pager, the frontier and the supervisor's derive."""
        self.admissions(20)
        self.business_turn_starts()
        for _ in range(3):
            self.tick(advance=TICK)
        short, _ = self.materialized(lambda: self.tick(advance=TICK))
        # Two hundred more settled admissions, admitted before the business turn, so the turn
        # derive evaluates and the frontier reads is the same one.
        evidence = self.store.one("SELECT evidence FROM generation_turns WHERE turn_id = ?",
                                  (BUSINESS,))["evidence"]
        for index in range(200):
            turn = f"late-{index:04}"
            self.store.db.execute(
                "INSERT INTO generation_turns (relationship_id, execution_generation, turn_id,"
                " evidence, actor, detail, admitted_at) VALUES (?,?,?,?,?,?,?)",
                (self.rid, 1, turn, evidence, "owner", "", "2000-01-01T00:00:00.000000+00:00"))
            self.settle(self.rid, CHILD, turn)
        for _ in range(3):
            self.tick(advance=TICK)
        long, _ = self.materialized(lambda: self.tick(advance=TICK))
        self.assertLess(long - short, 256, f"a tick read {short} bytes, then {long}")

    def test_the_newest_admission_is_one_index_seek(self):
        plan = [tuple(row) for row in self.store.db.execute(
            "EXPLAIN QUERY PLAN " + omitted.NEWEST_ADMISSION, (self.rid, 1, "x"))]
        details = " ".join(str(row[-1]) for row in plan)
        self.assertIn("USING INDEX", details, details)
        self.assertNotIn("TEMP B-TREE", details, details)


class TheHostListingItReads(DeliveryTestCase):
    def test_one_page_newest_first_mapped_to_thread_activity(self):
        from codex_session_relay.bridge_adapter import BridgeHostAdapter

        calls = []

        def call(method, params):
            calls.append((method, params))
            return {"data": [{"id": "a", "status": {"type": "idle"}, "updatedAt": 5},
                             {"id": "b", "status": {"type": "active", "activeFlags": []},
                              "updatedAt": 9},
                             {"status": {"type": "idle"}, "updatedAt": 3}],
                    "nextCursor": "more"}

        adapter = BridgeHostAdapter(call=call)
        page = adapter.recent_threads(50)
        self.assertEqual(calls, [("thread/list", {"limit": 50, "useStateDbOnly": True,
                                                  "sortKey": "updated_at",
                                                  "sortDirection": "desc"})])
        self.assertEqual([(one.thread_id, one.status, one.updated_at) for one in page.threads],
                         [("a", "idle", 5), ("b", "active", 9)])
        self.assertEqual(page.cursor, "more")
        adapter.recent_threads(50, cursor="more")
        self.assertEqual(calls[-1][1].get("cursor"), "more")






