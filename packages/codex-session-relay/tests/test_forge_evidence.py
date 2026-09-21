"""The collector answers the same way whether or not the agent reading it looked carefully.

Every case here is a shape a forge can really produce in which a careless read returns a number
that opens the merge gate. The originating one is the last thread on the last page: sixty of
sixty-three threads were read, none of the sixty was unresolved, and "0 unresolved" was reported
about a pull request with three open threads and a P1 among them.

The fixtures are scripted forge transcripts rather than mocks of this module's own functions. A
mock would assert that the code calls what it calls; a transcript asserts that the code reaches
the right verdict when the forge behaves in a way that has actually happened.
"""

import json
import os
import unittest
# By name, not as a module attribute: this module must stay one that does NOT spend real wall
# time, and the suite's own inventory decides that by reading subprocess.<call> sites.
from subprocess import TimeoutExpired

from codex_session_relay import forge, mergeevidence

HEAD = "a" * 40
BASE = "b" * 40
MOVED = "c" * 40


def pull(head=HEAD, base=BASE, **overrides):
    payload = {
        "number": 7, "html_url": "https://forge/pull/7", "state": "open", "merged": False,
        "draft": False, "head": {"sha": head}, "base": {"sha": base, "ref": "dev"},
        "mergeable": True, "mergeable_state": "clean",
    }
    payload.update(overrides)
    return payload


def threads(count, *, unresolved=(), start=1):
    return [
        {
            "id": "T" + str(start + index),
            "isResolved": (start + index) not in set(unresolved),
            "isOutdated": False, "path": "src/a.py", "line": index + 1,
            "comments": {"nodes": [{
                "url": "https://forge/thread/" + str(start + index),
                "author": {"login": "reviewer"}, "body": "look at this",
                "createdAt": "2026-09-22T00:00:00Z"}]},
        }
        for index in range(count)
    ]


def job(name, *, attempt=1, conclusion="success", status="completed", identifier=None,
        completed="2026-09-22T10:00:00Z"):
    return {
        "id": identifier if identifier is not None else abs(hash((name, attempt))) % 10 ** 9,
        "name": name, "run_attempt": attempt, "status": status, "conclusion": conclusion,
        "html_url": "https://forge/job", "started_at": "2026-09-22T09:00:00Z",
        "completed_at": completed,
    }


REQUIRED_DEV_GATE = [{
    "type": "required_status_checks",
    "parameters": {"strict_required_status_checks_policy": False,
                   "required_status_checks": [{"context": "dev-gate"}]},
}]


def workflow_run(identifier=1, *, workflow_id=100, event="pull_request", head=HEAD):
    """A workflow run. The workflow and the event are what decide which run replaced which."""
    return {"id": identifier, "name": "CI", "head_sha": head, "workflow_id": workflow_id,
            "event": event, "html_url": "https://forge/run/" + str(identifier)}


class Fake:
    """A forge transcript. Everything the collector can ask has a scripted answer here.

    Pagination is real rather than declared: the REST helper slices by the page and per_page the
    collector actually sent, and the GraphQL helper slices by first/after. A fixture that says
    "101 threads" therefore exercises the page boundary rather than asserting one.
    """

    def __init__(self, *, pulls=None, threads=(), second_threads=None, thread_script=None,
                 reviews=(), comments=(), runs=(), jobs=None, checks=(), statuses=(),
                 rules=None, rules_sequence=None, pull_status=None, ref_status=None,
                 rules_status=None):
        # Named rather than absorbed through setattr. A reflective write is a shape the suite's
        # own inventory cannot follow, and a fixture that reaches past a reader whose whole job
        # is to see every arming is not a fixture worth the convenience.
        self.pulls = list(pulls) if pulls else [pull()]
        self.threads = list(threads)
        self.second_threads = second_threads
        self.thread_script = thread_script
        self.reviews = list(reviews)
        self.comments = list(comments)
        self.runs = list(runs)
        self.jobs = dict(jobs or {})
        self.checks = list(checks)
        self.statuses = list(statuses)
        self.rules = list(REQUIRED_DEV_GATE if rules is None else rules)
        self.rules_sequence = rules_sequence
        self.pull_status = pull_status
        self.ref_status = ref_status
        self.rules_status = rules_status
        self.pull_reads = 0
        self.rules_reads = 0
        self.thread_passes = 0
        self.seen = []

    # ------------------------------------------------------------------ transport

    def run(self, argv, timeout):
        assert argv[0] == "gh", argv
        self.seen.append(list(argv))
        if argv[1:3] == ["api", "graphql"]:
            return self._graphql(argv)
        assert "--method" in argv and argv[argv.index("--method") + 1] == "GET", argv
        return self._rest(argv[-1])

    @staticmethod
    def _ok(payload):
        return 0, json.dumps(payload), ""

    @staticmethod
    def _fail(status):
        return 1, "", "gh: Not Found (HTTP " + str(status) + ")"

    @staticmethod
    def _query(target):
        _, _, query = target.partition("?")
        found = {}
        for pair in query.split("&"):
            key, _, value = pair.partition("=")
            if key:
                found[key] = value
        return found

    def _page(self, target, items, key):
        query = self._query(target)
        size = int(query.get("per_page", 100))
        page = int(query.get("page", 1))
        start = (page - 1) * size
        return self._ok({"total_count": len(items), key: items[start:start + size]})

    def _array(self, target, items):
        """A bare array endpoint, paginated the way the forge really paginates one."""
        query = self._query(target)
        size = int(query.get("per_page", 100))
        page = int(query.get("page", 1))
        start = (page - 1) * size
        return self._ok(list(items)[start:start + size])

    # --------------------------------------------------------------------- REST

    def _rest(self, target):
        path = target.partition("?")[0]
        if path.endswith("/pulls/7"):
            if self.pull_status:
                return self._fail(self.pull_status)
            answer = self.pulls[min(self.pull_reads, len(self.pulls) - 1)]
            self.pull_reads += 1
            return self._ok(answer)
        if "/git/ref/heads/" in path:
            if self.ref_status:
                return self._fail(self.ref_status)
            return self._ok({"ref": "refs/heads/dev"})
        if "/rules/branches/" in path:
            if self.rules_status:
                return self._fail(self.rules_status)
            if self.rules_sequence is not None:
                answer = self.rules_sequence[
                    min(self.rules_reads, len(self.rules_sequence) - 1)]
                self.rules_reads += 1
                return self._array(target, answer)
            return self._array(target, self.rules)
        if path.endswith("/actions/runs"):
            return self._page(target, self.runs, "workflow_runs")
        if "/actions/runs/" in path and path.endswith("/jobs"):
            run_id = int(path.split("/actions/runs/")[1].split("/")[0])
            return self._page(target, self.jobs.get(run_id, []), "jobs")
        if path.endswith("/check-runs"):
            return self._page(target, self.checks, "check_runs")
        if path.endswith("/status"):
            return self._page(target, self.statuses, "statuses")
        raise AssertionError("the collector asked for something unscripted: " + target)

    # ------------------------------------------------------------------ GraphQL

    def _graphql(self, argv):
        document = [one for one in argv if one.startswith("query=")][0]
        after = None
        first = 100
        for index, one in enumerate(argv):
            if one in ("-f", "-F") and argv[index + 1].startswith("after="):
                after = argv[index + 1][len("after="):]
            if one in ("-f", "-F") and argv[index + 1].startswith("first="):
                first = int(argv[index + 1][len("first="):])
        if "reviewThreads" in document:
            self.thread_passes += 1
            if self.thread_script is not None:
                return self._ok({"data": {"repository": {"pullRequest": {
                    "reviewThreads": self.thread_script(after, self.thread_passes)}}}})
            items = self.threads
            if self.second_threads is not None and self.thread_passes > len(
                    self._pages_for(self.threads, first)):
                items = self.second_threads
            return self._ok({"data": {"repository": {"pullRequest": {
                "reviewThreads": self._connection(items, after, first)}}}})
        key = "reviews" if "reviews(" in document else "comments"
        items = self.reviews if key == "reviews" else self.comments
        return self._ok({"data": {"repository": {"pullRequest": {
            key: self._connection(items, after, first)}}}})

    @staticmethod
    def _pages_for(items, first):
        return range(max(1, -(-len(items) // max(first, 1))))

    @staticmethod
    def _connection(items, after, first, total=None):
        start = 0 if not after else int(after[1:])
        page = items[start:start + first]
        end = start + len(page)
        more = end < len(items)
        return {
            "totalCount": len(items) if total is None else total,
            "pageInfo": {"hasNextPage": more, "endCursor": ("c" + str(end)) if more else None},
            "nodes": page,
        }


def collect(fake, **kwargs):
    engine = forge.Forge(run=fake.run, **kwargs)
    return forge.collect(engine, repository="owner/name", number=7)


def codes(snapshot):
    return [one["code"] for one in snapshot["problems"]]


def green_run(name="dev-gate", **kwargs):
    return {"runs": [workflow_run(1)], "jobs": {1: [job(name, **kwargs)]}}


def full_record(snapshot, **overrides):
    """The handoff exactly as the collector produced it, which is what a child hands over."""
    record = dict(snapshot["handoff"])
    record.update(overrides)
    return record


class TheLastPageIsRead(unittest.TestCase):
    """The defect this module exists for: a count read off a truncated prefix.

    Sixty of sixty-three threads, none of the sixty unresolved, reported as zero. The number that
    opens the gate has to come from a read that reached the end, and the end has to be proven
    rather than assumed.
    """

    def test_one_hundred_and_one_threads_cross_the_page_boundary(self):
        fake = Fake(threads=threads(101), **green_run())
        snapshot = collect(fake)
        coverage = snapshot["handoff"]["reviewCoverage"]
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        self.assertEqual(coverage["totalCount"], 101)
        self.assertEqual(len(coverage["threadsSeen"]), 101)
        self.assertFalse(coverage["hasNextPage"])
        self.assertEqual(coverage["pagesRead"], 2)

    def test_the_unresolved_thread_on_the_third_page_still_refuses(self):
        # 201 threads and the open one is the last. A reader that stopped at any earlier page
        # would have seen nothing unresolved and reported zero, which is the original failure.
        fake = Fake(threads=threads(201, unresolved=(201,)), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertEqual(snapshot["handoff"]["reviewCoverage"]["unresolved"], 1)
        self.assertIn(mergeevidence.REVIEW_INCOMPLETE, codes(snapshot))

    def test_a_budget_that_runs_out_is_unknown_and_never_zero(self):
        fake = Fake(threads=threads(500), **green_run())
        snapshot = collect(fake, page_budget=2)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.TRUNCATED, codes(snapshot))

    def test_a_cursor_that_repeats_stops_rather_than_spins(self):
        def script(after, _pass):
            return {"totalCount": 300, "pageInfo": {"hasNextPage": True, "endCursor": "same"},
                    "nodes": threads(100)}

        snapshot = collect(Fake(thread_script=script, **green_run()))
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.NOT_PROGRESSING, codes(snapshot))

    def test_a_total_that_moves_between_pages_is_not_a_set(self):
        pages = [
            {"totalCount": 150, "pageInfo": {"hasNextPage": True, "endCursor": "c100"},
             "nodes": threads(100)},
            {"totalCount": 149, "pageInfo": {"hasNextPage": False, "endCursor": None},
             "nodes": threads(49, start=101)},
        ]

        def script(after, _pass):
            return pages[0] if after is None else pages[1]

        snapshot = collect(Fake(thread_script=script, **green_run()))
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.TOTAL_MOVED, codes(snapshot))

    def test_a_repeated_identifier_is_not_a_second_thread(self):
        duplicated = threads(3) + threads(1)
        snapshot = collect(Fake(threads=duplicated, **green_run()))
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.DUPLICATED, codes(snapshot))


class ZeroPaysForASecondLook(unittest.TestCase):
    """A stable total does not prove a stable set, and zero is the answer that opens the gate."""

    def test_a_thread_swapped_between_passes_is_caught(self):
        # One deleted, one added: the total is unchanged and so is the number of distinct ids.
        fake = Fake(threads=threads(3), second_threads=threads(2) + threads(1, start=9),
                    **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.REVIEW_UNSTABLE, codes(snapshot))

    def test_a_thread_that_reopens_between_passes_is_caught(self):
        fake = Fake(threads=threads(3), second_threads=threads(3, unresolved=(2,)), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.REVIEW_UNSTABLE, codes(snapshot))

    def test_a_non_zero_count_does_not_pay_for_a_second_pass(self):
        # The confirming pass exists to protect the gate-opening answer. A refusal is already
        # safe, and doubling the cost of every refusal would buy nothing.
        fake = Fake(threads=threads(2, unresolved=(1,)), **green_run())
        collect(fake)
        thread_queries = [one for one in fake.seen
                          if one[1:3] == ["api", "graphql"]
                          and any("reviewThreads" in part for part in one)]
        self.assertEqual(len(thread_queries), 1)


class TheCandidateCanMoveWhileItIsRead(unittest.TestCase):
    def test_a_head_that_moves_mid_collection_is_stale(self):
        fake = Fake(pulls=[pull(), pull(head=MOVED)], threads=threads(2), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.STALE)
        self.assertIn(forge.CANDIDATE_MOVED, codes(snapshot))

    def test_a_base_that_moves_mid_collection_is_stale(self):
        fake = Fake(pulls=[pull(), pull(base=MOVED)], threads=threads(2), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.STALE)

    def test_rules_that_change_mid_collection_are_stale(self):
        """Pinning the head while the GATES move leaves every check unchanged and wrong."""
        fake = Fake(threads=threads(1), rules_sequence=[
            REQUIRED_DEV_GATE,
            [{"type": "required_status_checks",
              "parameters": {"required_status_checks": [{"context": "security-gate"}]}}],
        ], **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.STALE)
        self.assertIn(forge.GATES_MOVED, codes(snapshot))


class ChecksAreNotCollapsedByName(unittest.TestCase):
    """Two workflow runs publishing one name is a real shape, observed on this repository."""

    def test_a_pending_newer_run_is_not_hidden_by_an_older_success(self):
        fake = Fake(
            threads=threads(1),
            # Two DIFFERENT workflows, so neither replaced the other and both bind.
            runs=[workflow_run(1, workflow_id=100), workflow_run(2, workflow_id=200)],
            jobs={1: [job("dev-gate", identifier=11)],
                  2: [job("dev-gate", identifier=22, status="in_progress", conclusion=None)]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CHECKS_STALE, codes(snapshot))
        entries = snapshot["handoff"]["checks"]
        self.assertEqual(len({one["runId"] for one in entries}), 2)
        self.assertIn("in_progress", [one["conclusion"] for one in entries])

    def test_an_older_attempt_that_finished_later_does_not_outrank_a_newer_one(self):
        # Ordering by completion time would pick the 10:05 success over the 10:00 failure. The
        # rule is the attempt NUMBER, because a re-run is how a red check becomes green.
        fake = Fake(
            threads=threads(1),
            runs=[workflow_run(1)],
            jobs={1: [
                job("dev-gate", attempt=1, identifier=11, completed="2026-09-22T10:05:00Z"),
                job("dev-gate", attempt=2, identifier=12, conclusion="failure",
                    completed="2026-09-22T10:00:00Z"),
            ]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CHECKS_STALE, codes(snapshot))

    def test_a_rerun_still_running_does_not_inherit_the_old_success(self):
        fake = Fake(
            threads=threads(1),
            runs=[workflow_run(1)],
            jobs={1: [job("dev-gate", attempt=1, identifier=11),
                      job("dev-gate", attempt=2, identifier=12, status="queued",
                          conclusion=None)]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)

    def test_a_job_not_rerun_in_a_later_attempt_is_not_read_as_missing(self):
        # Keying the attempt to the workflow run rather than the job would grade this job at an
        # attempt it never had, and it would go missing from the required set.
        fake = Fake(
            threads=threads(1),
            runs=[workflow_run(1)],
            jobs={1: [job("dev-gate", attempt=1, identifier=11),
                      job("lint", attempt=2, identifier=12)]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])

    def test_a_truncated_check_page_is_unknown_rather_than_green(self):
        many = [{"id": 900 + index, "name": "noise", "head_sha": HEAD, "status": "completed",
                 "conclusion": "success", "app": {"slug": "other"}} for index in range(150)]
        fake = Fake(threads=threads(1), checks=many, **green_run())
        self.assertEqual(collect(fake, page_budget=1)["verdict"], forge.UNKNOWN)

    def test_a_failing_namesake_on_the_second_check_page_is_found(self):
        many = [{"id": 900 + index, "name": "noise", "head_sha": HEAD, "status": "completed",
                 "conclusion": "success", "app": {"slug": "other"}} for index in range(120)]
        many.append({"id": 5000, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                     "conclusion": "failure", "app": {"slug": "other"}})
        fake = Fake(threads=threads(1), checks=many, **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CHECKS_STALE, codes(snapshot))

    def test_a_truncated_workflow_run_page_is_unknown(self):
        runs = [workflow_run(index, workflow_id=index) for index in range(1, 151)]
        fake = Fake(threads=threads(1), runs=runs,
                    jobs={index: [job("dev-gate", identifier=index)] for index in range(1, 151)})
        self.assertEqual(collect(fake, page_budget=1)["verdict"], forge.UNKNOWN)

    def test_a_pending_commit_status_is_not_success(self):
        fake = Fake(threads=threads(1),
                    statuses=[{"context": "dev-gate", "state": "pending",
                               "target_url": "https://forge/s"}],
                    rules=REQUIRED_DEV_GATE)
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)


class DeclaredEmptyIsNotUnknown(unittest.TestCase):
    def test_unreadable_rules_leave_the_required_set_undeclared(self):
        fake = Fake(threads=threads(1), rules_status=403, **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIs(snapshot["handoff"]["requiredDeclared"], mergeevidence.UNDECLARED)
        self.assertIn(forge.UNREADABLE, codes(snapshot))

    def test_a_branch_with_no_rules_declares_an_empty_set(self):
        fake = Fake(threads=threads(1), rules=[], **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        self.assertEqual(snapshot["handoff"]["requiredDeclared"], [])

    def test_an_empty_answer_about_a_branch_that_is_gone_is_stale(self):
        # The endpoint answers the same way for a branch with no rules and a branch that does not
        # exist. Read as "requires nothing", a vanished base lets one optional green check pass.
        fake = Fake(threads=threads(1), rules=[], ref_status=404, **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.STALE)
        self.assertIn(forge.BASE_REF_MISSING, codes(snapshot))
        self.assertIs(snapshot["handoff"]["requiredDeclared"], mergeevidence.UNDECLARED)


class TheDisabledReviewerIsNotAGate(unittest.TestCase):
    def test_its_failing_check_does_not_block_when_nothing_requires_it(self):
        fake = Fake(threads=threads(1),
                    checks=[{"id": 77, "name": "codex", "head_sha": HEAD, "status": "completed",
                             "conclusion": "failure", "app": {"slug": "codex"}}],
                    **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])

    def test_a_rule_that_still_requires_it_is_reported_rather_than_bypassed(self):
        fake = Fake(threads=threads(1), rules=[{
            "type": "required_status_checks",
            "parameters": {"required_status_checks": [{"context": "dev-gate"},
                                                      {"context": "codex"}]}}], **green_run())
        snapshot = collect(fake)
        self.assertIn(forge.GATE_CONFLICT, codes(snapshot))
        self.assertIn("codex", snapshot["handoff"]["requiredDeclared"])
        self.assertNotEqual(snapshot["verdict"], forge.READY)


class TheCandidateItself(unittest.TestCase):
    def test_a_closed_candidate_is_not_ready_however_green(self):
        fake = Fake(pulls=[pull(state="closed")], threads=threads(1), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CANDIDATE_NOT_OPEN, codes(snapshot))

    def test_a_conflicted_candidate_is_not_ready(self):
        fake = Fake(pulls=[pull(mergeable_state="dirty")], threads=threads(1), **green_run())
        self.assertIn(mergeevidence.CANDIDATE_CONFLICTED, codes(collect(fake)))

    def test_an_unstable_merge_state_with_a_failing_optional_check_is_still_ready(self):
        """UNSTABLE says something is failing, not that the something is required.

        checks_problems deliberately lets an optional lint fail beside a green required gate.
        Refusing UNSTABLE would contradict that rule from inside the same module.
        """
        fake = Fake(
            pulls=[pull(mergeable_state="unstable")], threads=threads(1),
            runs=[workflow_run(1)],
            jobs={1: [job("dev-gate", identifier=11),
                      job("lint", identifier=12, conclusion="failure")]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])

    def test_behind_refuses_only_where_the_branch_requires_currency(self):
        lenient = Fake(pulls=[pull(mergeable_state="behind")], threads=threads(1), **green_run())
        self.assertEqual(collect(lenient)["verdict"], forge.READY)
        strict = Fake(pulls=[pull(mergeable_state="behind")], threads=threads(1),
                      rules=[{"type": "required_status_checks",
                              "parameters": {"strict_required_status_checks_policy": True,
                                             "required_status_checks": [{"context": "dev-gate"}]}}],
                      **green_run())
        snapshot = collect(strict)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CANDIDATE_BEHIND, codes(snapshot))

    def test_an_unrecognised_merge_state_is_unknown_rather_than_clean(self):
        fake = Fake(pulls=[pull(mergeable_state="marvellous")], threads=threads(1), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)

    def test_a_draft_is_refused_in_the_grader_s_own_words(self):
        fake = Fake(pulls=[pull(draft=True)], threads=threads(1), **green_run())
        snapshot = collect(fake)
        self.assertIn(mergeevidence.CANDIDATE_DRAFT, codes(snapshot))
        self.assertTrue(snapshot["handoff"]["isDraft"])


class TheRecordMustSurviveAReadingItDidNotProduce(unittest.TestCase):
    def test_a_thread_that_arrived_late_on_an_unchanged_head_invalidates_the_record(self):
        fake = Fake(threads=threads(2), **green_run())
        snapshot = collect(fake)
        coverage = dict(snapshot["handoff"]["reviewCoverage"])
        coverage.update(threadsSeen=["T1"], totalCount=1)
        record = full_record(snapshot, reviewCoverage=coverage)
        problems = forge.restate_problems(HEAD, record, snapshot)
        self.assertEqual([one.code for one in problems], [forge.LATE_FINDING])

    def test_a_record_about_another_head_is_not_current(self):
        fake = Fake(threads=threads(1), **green_run())
        snapshot = collect(fake)
        problems = forge.restate_problems(MOVED, full_record(snapshot), snapshot)
        self.assertIn(forge.CANDIDATE_MOVED, [one.code for one in problems])

    def test_a_record_that_still_describes_the_candidate_raises_nothing(self):
        fake = Fake(threads=threads(2), **green_run())
        snapshot = collect(fake)
        self.assertEqual(forge.restate_problems(HEAD, full_record(snapshot), snapshot), [])

    def test_an_empty_record_does_not_pass_for_want_of_a_late_thread(self):
        """Comparing an unvalidated record says only that it does not disagree with the forge.

        On a pull request with no threads there is nothing that can be late, so a restatement
        that looked only for late threads accepted a caller who had collected nothing at all.
        """
        fake = Fake(threads=(), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        problems = forge.restate_problems(HEAD, {}, snapshot)
        self.assertTrue(problems)
        # Shape first, as the handoff contract orders it: a record with no review coverage at
        # all is malformed rather than merely incomplete, and reading its absent fields with
        # coercion is how nothing passed for satisfied in the first place.
        self.assertIn(mergeevidence.MALFORMED, [one.code for one in problems])

    def test_a_base_that_moved_under_an_unchanged_head_is_not_current(self):
        # Every merge-result check the record carries was computed against the old base, and a
        # branch requiring currency will not take them. The head alone cannot notice this.
        fake = Fake(threads=threads(1), **green_run())
        snapshot = collect(fake)
        record = full_record(snapshot, baseSha=MOVED)
        problems = forge.restate_problems(HEAD, record, snapshot)
        self.assertIn(forge.CANDIDATE_MOVED, [one.code for one in problems])

    def test_a_record_that_never_named_its_base_says_so(self):
        fake = Fake(threads=threads(1), **green_run())
        snapshot = collect(fake)
        record = full_record(snapshot)
        record.pop("baseSha")
        self.assertIn(forge.RECORD_INVALID,
                      [one.code for one in forge.restate_problems(HEAD, record, snapshot)])


class NothingHereWrites(unittest.TestCase):
    def test_a_mutation_document_is_refused_before_it_is_sent(self):
        engine = forge.Forge(run=Fake().run)
        with self.assertRaises(forge.ForgeUsage):
            engine.graphql("mutation { resolveReviewThread }", "anything")

    def test_every_rest_call_states_the_get_method(self):
        fake = Fake(threads=threads(2), **green_run())
        collect(fake)
        rest = [one for one in fake.seen if one[1:3] != ["api", "graphql"]]
        self.assertTrue(rest)
        for argv in rest:
            self.assertIn("--method", argv)
            self.assertEqual(argv[argv.index("--method") + 1], "GET")

    def test_an_argument_that_could_be_read_as_an_option_never_reaches_argv(self):
        for repository in ("--owner/name", "owner name/x", "owner", "own/er/name", ""):
            with self.assertRaises(forge.ForgeUsage):
                forge.split_repository(repository)
        for number in (0, -1, "seven", None):
            with self.assertRaises(forge.ForgeUsage):
                forge.pull_request_number(number)
        for branch in ("../etc", "/dev", "de v", "dev?x", ""):
            with self.assertRaises(forge.ForgeUsage):
                forge.branch_ref(branch)
        self.assertEqual(forge.branch_ref("release/1.2"), "release/1.2")

    def test_an_unreadable_pull_request_collects_nothing_and_says_so(self):
        fake = Fake(pull_status=404)
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.UNREADABLE, codes(snapshot))


if __name__ == "__main__":
    unittest.main()


class ARunThatWasReplacedDoesNotBlockTheOneThatReplacedIt(unittest.TestCase):
    """Observed on this issue's own pull request, by this collector reading it.

    The repository's CI cancels an in-progress run when a new event arrives for the same pull
    request. That leaves a cancelled run whose dev-gate failed sitting on the same head as the
    successful one, and grading them as peers refuses a candidate whose current CI is green.
    A re-run and a cancellation are the same relation seen twice: the newer one answers.
    """

    def test_a_cancelled_earlier_run_of_the_same_workflow_is_not_graded(self):
        fake = Fake(
            threads=threads(1),
            runs=[workflow_run(1, workflow_id=100), workflow_run(2, workflow_id=100)],
            jobs={1: [job("dev-gate", identifier=11, conclusion="cancelled")],
                  2: [job("dev-gate", identifier=22)]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        self.assertEqual([one["runId"] for one in snapshot["handoff"]["checks"]],
                         ["workflow-run:2:dev-gate#0"])
        self.assertEqual([one["runId"] for one in snapshot["supersededRuns"]], ["1"])

    def test_the_newest_run_still_decides_when_it_is_the_failing_one(self):
        # The rule is "the newest answers", not "the green one answers". A newer run that failed
        # replaces an older success exactly as it would replace an older failure.
        fake = Fake(
            threads=threads(1),
            runs=[workflow_run(1, workflow_id=100), workflow_run(2, workflow_id=100)],
            jobs={1: [job("dev-gate", identifier=11)],
                  2: [job("dev-gate", identifier=22, conclusion="failure")]},
        )
        self.assertEqual(collect(fake)["verdict"], forge.NOT_READY)

    def test_a_different_event_is_not_a_replacement(self):
        fake = Fake(
            threads=threads(1),
            runs=[workflow_run(1, workflow_id=100, event="push"),
                  workflow_run(2, workflow_id=100, event="pull_request")],
            jobs={1: [job("dev-gate", identifier=11, conclusion="failure")],
                  2: [job("dev-gate", identifier=22)]},
        )
        self.assertEqual(collect(fake)["verdict"], forge.NOT_READY)


class ARequiredContextCanNameItsProvider(unittest.TestCase):
    """A rule binds a context to an integration, and a namesake from elsewhere is not it."""

    RULES = [{"type": "required_status_checks",
              "parameters": {"required_status_checks": [
                  {"context": "dev-gate", "integration_id": 42}]}}]

    def test_a_namesake_from_another_app_does_not_answer_for_the_gate(self):
        fake = Fake(
            threads=threads(1), rules=self.RULES, runs=[], jobs={},
            checks=[{"id": 9, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                     "conclusion": "success", "app": {"id": 99, "slug": "impostor"}}],
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CHECKS_STALE, codes(snapshot))
        self.assertEqual(snapshot["handoff"]["requiredProviders"], {"dev-gate": ["42"]})

    def test_two_rules_binding_one_context_both_have_to_answer(self):
        # Assigned rather than accumulated, the second rule erased the first and one app's
        # success satisfied a gate the other app never ran.
        rules = [
            {"type": "required_status_checks",
             "parameters": {"required_status_checks": [
                 {"context": "dev-gate", "integration_id": 42}]}},
            {"type": "required_status_checks",
             "parameters": {"required_status_checks": [
                 {"context": "dev-gate", "integration_id": 77}]}},
        ]
        published = [{"id": 9, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                      "conclusion": "success", "app": {"id": 42, "slug": "one"}}]
        half = Fake(threads=threads(1), rules=rules, runs=[], jobs={}, checks=published)
        snapshot = collect(half)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertEqual(snapshot["handoff"]["requiredProviders"], {"dev-gate": ["42", "77"]})
        both = Fake(threads=threads(1), rules=rules, runs=[], jobs={}, checks=published + [
            {"id": 10, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
             "conclusion": "success", "app": {"id": 77, "slug": "two"}}])
        self.assertEqual(collect(both)["verdict"], forge.READY)

    def test_the_declared_provider_answers_for_it(self):
        fake = Fake(
            threads=threads(1), rules=self.RULES, runs=[], jobs={},
            checks=[{"id": 9, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                     "conclusion": "success", "app": {"id": 42, "slug": "actions"}}],
        )
        self.assertEqual(collect(fake)["verdict"], forge.READY)

    def test_a_workflow_job_carries_the_app_the_check_run_states(self):
        # The jobs endpoint does not name the publishing app; the check-runs endpoint describes
        # the same objects and does. Without the join a real required gate looks like an impostor.
        fake = Fake(
            threads=threads(1), rules=self.RULES, **green_run(identifier=11),
            checks=[{"id": 11, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                     "conclusion": "success", "app": {"id": 42, "slug": "actions"}}],
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        self.assertEqual([one["provider"] for one in snapshot["handoff"]["checks"]], ["42"])


class TheCandidateCanChangeWithoutMovingACommit(unittest.TestCase):
    """Closing it, drafting it or retargeting it leaves both shas untouched."""

    def drifted(self, **after):
        fake = Fake(pulls=[pull(), pull(**after)], threads=threads(1), **green_run())
        return collect(fake)

    def test_becoming_a_draft_mid_collection_is_stale(self):
        snapshot = self.drifted(draft=True)
        self.assertEqual(snapshot["verdict"], forge.STALE)
        self.assertIn(forge.CANDIDATE_MOVED, codes(snapshot))

    def test_closing_mid_collection_is_stale(self):
        self.assertEqual(self.drifted(state="closed")["verdict"], forge.STALE)

    def test_retargeting_to_another_branch_at_the_same_commit_is_stale(self):
        fake = Fake(pulls=[pull(), pull(base={"sha": BASE, "ref": "main"})], threads=threads(1),
                    **green_run())
        self.assertEqual(collect(fake)["verdict"], forge.STALE)

    def test_a_merge_state_that_settles_during_the_read_is_not_movement(self):
        """The forge computes it asynchronously, so it routinely settles from unknown.

        Calling that movement would make nearly every collection stale. The re-read is simply
        the better answer about the same candidate, so it is the one graded.
        """
        fake = Fake(pulls=[pull(mergeable_state="unknown"), pull(mergeable_state="clean")],
                    threads=threads(1), **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])

    def test_a_merge_state_that_settles_into_a_refusal_is_graded_too(self):
        fake = Fake(pulls=[pull(mergeable_state="unknown"), pull(mergeable_state="dirty")],
                    threads=threads(1), **green_run())
        self.assertIn(mergeevidence.CANDIDATE_CONFLICTED, codes(collect(fake)))


class AReviewerWhoAskedForChangesIsStillWaiting(unittest.TestCase):
    """Enumerating reviews and grading none of them is collecting evidence and not using it."""

    @staticmethod
    def review(state, *, author="reviewer", when="2026-09-22T10:00:00Z", identifier="R1"):
        return {"id": identifier, "state": state, "url": "https://forge/review", "body": "",
                "submittedAt": when, "author": {"login": author}}

    def test_an_outstanding_changes_requested_review_refuses(self):
        fake = Fake(threads=threads(1), reviews=[self.review("CHANGES_REQUESTED")], **green_run())
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CHANGES_REQUESTED, codes(snapshot))

    def test_a_later_approval_from_the_same_reviewer_answers_it(self):
        fake = Fake(threads=threads(1), **green_run(), reviews=[
            self.review("CHANGES_REQUESTED", when="2026-09-22T10:00:00Z", identifier="R1"),
            self.review("APPROVED", when="2026-09-22T11:00:00Z", identifier="R2"),
        ])
        self.assertEqual(collect(fake)["verdict"], forge.READY)

    def test_a_comment_neither_blocks_nor_clears(self):
        fake = Fake(threads=threads(1), **green_run(), reviews=[
            self.review("CHANGES_REQUESTED", when="2026-09-22T10:00:00Z", identifier="R1"),
            self.review("COMMENTED", when="2026-09-22T11:00:00Z", identifier="R2"),
        ])
        self.assertEqual(collect(fake)["verdict"], forge.NOT_READY)

    def test_another_reviewer_s_approval_does_not_answer_for_the_first(self):
        fake = Fake(threads=threads(1), **green_run(), reviews=[
            self.review("CHANGES_REQUESTED", author="anna", identifier="R1"),
            self.review("APPROVED", author="bo", when="2026-09-22T11:00:00Z", identifier="R2"),
        ])
        self.assertEqual(collect(fake)["verdict"], forge.NOT_READY)


class AForgeThatNeverAnswersIsUnknown(unittest.TestCase):
    def test_a_timeout_is_an_observation_that_did_not_happen(self):
        """Exit 3 and a traceback throws away every connection already read.

        The honest answer is the snapshot, naming the one part nobody could see.
        """
        def timing_out(argv, timeout):
            if "check-runs" in argv[-1]:
                raise TimeoutExpired(argv, timeout)
            return Fake(threads=threads(1), **green_run()).run(argv, timeout)

        engine = forge.Forge(run=timing_out)
        snapshot = forge.collect(engine, repository="owner/name", number=7)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.UNREADABLE, codes(snapshot))
        self.assertTrue(any("timeout" in one["detail"] for one in snapshot["problems"]))


class TwoJobsCanShareOneName(unittest.TestCase):
    """Keyed by name alone, two jobs in one run merge into one identity.

    The attempt rule then does the damage it exists to prevent: the higher-attempt success of
    one job stands for the other job's failure, under a runId that names them both.
    """

    def test_a_namesake_in_the_same_run_does_not_absorb_the_other_s_failure(self):
        fake = Fake(
            threads=threads(1), runs=[workflow_run(1)],
            jobs={1: [job("dev-gate", identifier=11, attempt=1),
                      job("dev-gate", identifier=12, attempt=1, conclusion="failure")]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertEqual(len({one["runId"] for one in snapshot["handoff"]["checks"]}), 2)

    def test_attempts_of_one_job_still_meet_under_one_identity(self):
        fake = Fake(
            threads=threads(1), runs=[workflow_run(1)],
            jobs={1: [job("dev-gate", identifier=11, attempt=1, conclusion="failure"),
                      job("dev-gate", identifier=12, attempt=2)]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        self.assertEqual(len({one["runId"] for one in snapshot["handoff"]["checks"]}), 1)


class RecencyIsNotAnAssumptionAboutIdentifiers(unittest.TestCase):
    def test_the_run_the_forge_started_later_is_the_one_that_answers(self):
        older = dict(workflow_run(900, workflow_id=100), run_started_at="2026-09-22T09:00:00Z")
        newer = dict(workflow_run(2, workflow_id=100), run_started_at="2026-09-22T11:00:00Z")
        fake = Fake(
            threads=threads(1), runs=[older, newer],
            jobs={900: [job("dev-gate", identifier=11, conclusion="failure")],
                  2: [job("dev-gate", identifier=22)]},
        )
        snapshot = collect(fake)
        self.assertEqual(snapshot["verdict"], forge.READY, snapshot["problems"])
        self.assertEqual([one["runId"] for one in snapshot["supersededRuns"]], ["900"])


class AGateSetCanMoveBetweenTheRecordAndTheRestatement(unittest.TestCase):
    def test_a_branch_that_added_a_gate_invalidates_the_record(self):
        fake = Fake(threads=threads(1), **green_run())
        snapshot = collect(fake)
        record = full_record(snapshot, requiredDeclared=["dev-gate", "security-gate"])
        problems = forge.restate_problems(HEAD, record, snapshot)
        self.assertIn(forge.GATES_MOVED, [one.code for one in problems])

    def test_a_disagreement_about_the_provider_invalidates_it_too(self):
        fake = Fake(threads=threads(1), rules=[{
            "type": "required_status_checks",
            "parameters": {"required_status_checks": [
                {"context": "dev-gate", "integration_id": 42}]}}], runs=[], jobs={},
            checks=[{"id": 9, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                     "conclusion": "success", "app": {"id": 42, "slug": "actions"}}])
        snapshot = collect(fake)
        record = full_record(snapshot, requiredProviders={"dev-gate": "99"})
        self.assertIn(forge.GATES_MOVED,
                      [one.code for one in forge.restate_problems(HEAD, record, snapshot)])

    def test_a_provider_map_of_the_wrong_shape_is_refused_not_raised(self):
        fake = Fake(threads=threads(1), **green_run())
        snapshot = collect(fake)
        record = full_record(snapshot, requiredProviders=["dev-gate"])
        problems = forge.restate_problems(HEAD, record, snapshot)
        self.assertIn(forge.RECORD_INVALID, [one.code for one in problems])


    def test_a_record_that_never_read_the_providers_cannot_answer_a_bound_gate(self):
        # Absence reading as agreement is the failure this whole module exists for. A record
        # that never read the integration out of the rule has not satisfied a gate bound to one.
        fake = Fake(threads=threads(1), rules=[{
            "type": "required_status_checks",
            "parameters": {"required_status_checks": [
                {"context": "dev-gate", "integration_id": 42}]}}], runs=[], jobs={},
            checks=[{"id": 9, "name": "dev-gate", "head_sha": HEAD, "status": "completed",
                     "conclusion": "success", "app": {"id": 42, "slug": "actions"}}])
        snapshot = collect(fake)
        record = full_record(snapshot)
        record.pop("requiredProviders")
        self.assertIn(forge.GATES_MOVED,
                      [one.code for one in forge.restate_problems(HEAD, record, snapshot)])


class TheBranchRulesAreAListLikeAnyOther(unittest.TestCase):
    """The one connection that was not routed through the enumerator, which is the whole subject.

    Read with a single request, the effective-rules endpoint silently answers with its first
    page. A gate declared by a later ruleset is then absent from the required set, and the
    candidate missing that gate reports ready - the originating defect, in the list that decides
    what "required" even means.
    """

    @staticmethod
    def noise(count):
        return [{"type": "deletion", "ruleset_id": index} for index in range(count)]

    def test_a_required_gate_on_the_second_page_is_still_required(self):
        rules = self.noise(100) + REQUIRED_DEV_GATE
        fake = Fake(threads=threads(1), rules=rules, runs=[], jobs={}, checks=[])
        snapshot = collect(fake)
        self.assertEqual(snapshot["handoff"]["requiredDeclared"], ["dev-gate"])
        self.assertEqual(snapshot["verdict"], forge.NOT_READY)
        self.assertIn(mergeevidence.CHECKS_STALE, codes(snapshot))

    def test_a_rules_read_that_runs_out_of_budget_declares_nothing(self):
        # Refusing to declare is the point: a prefix of the rules is not the rules, and a
        # required set built from one is missing exactly the gate nobody read.
        rules = self.noise(400) + REQUIRED_DEV_GATE
        fake = Fake(threads=threads(1), rules=rules, **green_run())
        snapshot = collect(fake, page_budget=2)
        self.assertEqual(snapshot["verdict"], forge.UNKNOWN)
        self.assertIn(forge.TRUNCATED, codes(snapshot))
        self.assertIs(snapshot["handoff"]["requiredDeclared"], mergeevidence.UNDECLARED)

    def test_the_rules_connection_is_recorded_like_the_others(self):
        fake = Fake(threads=threads(1), **green_run())
        named = [one["connection"] for one in collect(fake)["connections"]]
        self.assertIn("effective branch rules", named)

