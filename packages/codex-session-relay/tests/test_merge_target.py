"""Where a merge target's base branch points, read from real repositories (CRW-229).

The merge turn records a base only from a reading of the target. These cases build bare
repositories with git and read them through the relay's own reader and its command line,
because what a real git answers for a ref - and what it refuses - is the property under test,
and a scripted runner would only repeat what this file already assumes. That makes this module
one that spends real time: it starts git, and it is declared in test_regression_map.py.

The forge half is the exception. Reaching GitHub from a test is exactly what the suite must not
do, so those cases drive forge.Forge with a scripted runner, as test_forge_evidence.py does.
"""

import io
import json
import os
import sqlite3
import subprocess
from contextlib import redirect_stdout
from unittest.mock import patch

from codex_session_relay import cli, forge
from codex_session_relay.mergetarget import (
    GITHUB, LOCAL_GIT, TargetReader, TargetUnreadable, same_commit,
)

from .support import RelayTestCase

IDENTITY = {
    "GIT_AUTHOR_NAME": "relay test", "GIT_AUTHOR_EMAIL": "relay@test.invalid",
    "GIT_COMMITTER_NAME": "relay test", "GIT_COMMITTER_EMAIL": "relay@test.invalid",
    "GIT_AUTHOR_DATE": "2026-09-24T00:00:00Z", "GIT_COMMITTER_DATE": "2026-09-24T00:00:00Z",
    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
}
GREEN = json.dumps({"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
                    "threadsSeen": ["thread-1"], "unresolved": 0})


def git(*arguments, stdin=""):
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith("GIT_")}
    environment.update(IDENTITY)
    completed = subprocess.run(
        ["git", *arguments], input=stdin, capture_output=True, text=True, check=True,
        env=environment)
    return completed.stdout.strip()


class Repository:
    """A bare repository whose branches the case moves by hand, as a merge would."""

    def __init__(self, path):
        self.path = path
        git("init", "--quiet", "--bare", path)
        self.tree = git("--git-dir=" + path, "mktree")

    def commit(self, message, parent=None):
        extra = ["-p", parent] if parent else []
        return git("--git-dir=" + self.path, "commit-tree", self.tree, *extra, "-m", message)

    def point(self, branch, sha):
        git("--git-dir=" + self.path, "update-ref", "refs/heads/" + branch, sha)


class ReadingALocalRepository(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.repo = Repository(os.path.join(self.tmp, "R.git"))
        self.base = self.repo.commit("base")
        self.head = self.repo.commit("candidate", self.base)
        self.repo.point("main", self.base)
        self.reader = TargetReader()

    def unreadable(self, repository, branch):
        with self.assertRaises(TargetUnreadable) as caught:
            self.reader.tip(repository, branch)
        return caught.exception.detail

    def test_it_reads_the_branch_and_follows_it_when_it_moves(self):
        reading = self.reader.tip(self.repo.path, "main")
        self.assertEqual(reading["sha"], self.base)
        self.assertEqual(reading["source"], LOCAL_GIT)
        self.assertEqual(reading["reference"], "refs/heads/main")
        self.repo.point("main", self.head)
        self.assertEqual(self.reader.tip(self.repo.path, "main")["sha"], self.head)

    def test_revision_syntax_is_never_evaluated(self):
        """rev-parse answers main~1 with the parent - the very pre-landing base."""
        self.repo.point("main", self.head)
        for branch in ("main~1", "main^", "main@{1}", "-main", "main.lock", "ma*in", " main"):
            self.unreadable(self.repo.path, branch)

    def test_a_branch_that_does_not_exist_is_unreadable_with_gits_reason(self):
        self.assertIn("refs/heads/nope", self.unreadable(self.repo.path, "nope"))

    def test_only_an_absolute_path_or_owner_name_names_a_target(self):
        self.unreadable("R.git", "main")
        self.unreadable(os.path.join(self.tmp, "missing.git"), "main")

    def test_a_directory_that_is_not_a_repository_is_unreadable(self):
        plain = os.path.join(self.tmp, "plain")
        os.makedirs(plain)
        self.unreadable(plain, "main")

    def test_a_directory_inside_a_repository_does_not_read_the_enclosing_one(self):
        work = os.path.join(self.tmp, "work-repo")
        git("init", "--quiet", "-b", "main", work)
        git("-C", work, "commit", "--quiet", "--allow-empty", "-m", "enclosing")
        inner = os.path.join(work, "inner")
        os.makedirs(inner)
        self.unreadable(inner, "main")

    def test_a_working_repository_is_read_through_its_git_directory(self):
        work = os.path.join(self.tmp, "work-repo")
        git("init", "--quiet", "-b", "main", work)
        git("-C", work, "commit", "--quiet", "--allow-empty", "-m", "tip")
        self.assertEqual(self.reader.tip(work, "main")["sha"],
                         git("-C", work, "rev-parse", "HEAD"))

    def test_the_callers_git_environment_does_not_redirect_the_read(self):
        other = Repository(os.path.join(self.tmp, "other.git"))
        other.point("main", other.commit("elsewhere"))
        with patch.dict(os.environ, {"GIT_DIR": other.path, "GIT_NAMESPACE": "x"}):
            self.assertEqual(self.reader.tip(self.repo.path, "main")["sha"], self.base)


class ReadingAForge(RelayTestCase):
    """GitHub, through a scripted runner. No test here reaches a network."""

    SHA = "a" * 40

    def reader(self, *answers):
        self.calls = []
        script = list(answers)

        def run(argv, timeout):
            self.calls.append(argv)
            answer = script.pop(0)
            if isinstance(answer, BaseException):
                raise answer
            return answer

        return TargetReader(forge_factory=lambda: forge.Forge(run=run))

    def answer(self, **overrides):
        payload = {"ref": "refs/heads/dev", "object": {"type": "commit", "sha": self.SHA}}
        payload.update(overrides)
        return 0, json.dumps(payload), ""

    def test_it_reads_the_branch_with_one_get(self):
        reading = self.reader(self.answer()).tip("owner/repo", "dev")
        self.assertEqual(reading["sha"], self.SHA)
        self.assertTrue(reading["source"].startswith(GITHUB + ":"))
        (argv,) = self.calls
        self.assertIn("GET", argv)
        self.assertEqual(argv[-1], "repos/owner/repo/git/ref/heads/dev")

    def test_every_answer_that_does_not_name_this_branchs_commit_is_unreadable(self):
        answers = (
            (1, "", "gh: Not Found (HTTP 404)"),
            (1, "", "gh: Git Repository is empty. (HTTP 409)"),
            (0, json.dumps([{"ref": "refs/heads/dev"}]), ""),
            self.answer(ref="refs/heads/dev-2"),
            self.answer(object={"type": "tag", "sha": self.SHA}),
            self.answer(object={"type": "commit", "sha": "A" * 40}),
            FileNotFoundError("gh"),
        )
        for answer in answers:
            with self.assertRaises(TargetUnreadable, msg=repr(answer)):
                self.reader(answer).tip("owner/repo", "dev")

    def test_a_repository_that_is_not_owner_name_starts_nothing(self):
        for repository in ("-owner/repo", "owner", "owner/repo/extra", "https://x/y"):
            with self.assertRaises(TargetUnreadable):
                self.reader().tip(repository, "dev")
        self.assertEqual(self.calls, [])


class SameCommit(RelayTestCase):
    def test_names_match_in_full_in_any_case_and_never_by_prefix(self):
        full = "abcdef1234" + "0" * 30
        self.assertTrue(same_commit(full.upper(), full))
        self.assertFalse(same_commit("abcdef1", full))
        self.assertFalse(same_commit(full, "abcdef1"))
        self.assertFalse(same_commit("base-0", "base-00"))
        self.assertTrue(same_commit("base-0", "base-0"))
        self.assertFalse(same_commit("", ""))


class TheCommandLineAgainstARealTarget(RelayTestCase):
    """The CRW-124 G1 sequence, on a bare repository, through the commands a parent runs."""

    def setUp(self):
        super().setUp()
        self.state = str(self.store.path.parent)
        self.store.close()
        self.repo = Repository(os.path.join(self.tmp, "R-A.git"))
        self.base = self.repo.commit("skeleton")
        self.a1 = self.repo.commit("A1", self.base)
        self.a2 = self.repo.commit("A2", self.a1)
        self.repo.point("main", self.base)

    def run_cli(self, *arguments):
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = cli.main(["--state", self.state, *arguments])
        printed = buffer.getvalue().strip()
        return code, (json.loads(printed) if printed else None)

    def ok(self, *arguments):
        code, payload = self.run_cli(*arguments)
        self.assertEqual(code, cli.EXIT_OK, payload)
        return payload

    def refused(self, reason, *arguments):
        code, payload = self.run_cli(*arguments)
        self.assertEqual(code, cli.EXIT_REFUSED, payload)
        self.assertEqual(payload["reason"], reason)
        return payload

    def claim(self, head):
        claimed = self.ok(
            "merge-turn-request", "--repository", self.repo.path, "--base-ref", "main",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a", "--head", head,
            "--ready")
        self.ok("merge-turn-acknowledge", "--turn", claimed["turnId"], "--actor", "task-alpha",
                "--grant", claimed["grant"]["grantId"], "--evidence", "read the grant")
        return claimed["turnId"]

    def check(self, turn, head, base):
        checks = json.dumps([{"runId": "run-" + head[:7], "name": "required", "headSha": head,
                              "conclusion": "success", "attempt": 1}])
        return ("merge-turn-check", "--turn", turn, "--actor", "task-alpha",
                "--head-sha", head, "--base-sha", base, "--checks", checks,
                "--review", GREEN, "--required", "required")

    def test_the_trial_is_refused_where_it_went_wrong_and_recovers_by_restating(self):
        self.ok("linkage-bind", "--role", "parent", "--scope", "PRJ-A", "--task", "task-alpha",
                "--host", "host-a")
        first = self.claim(self.a1)
        self.assertEqual(self.ok(*self.check(first, self.a1, self.base))["state"], "merging")

        # The parent fast-forwards main to A1, then states the base it had checked against.
        self.repo.point("main", self.a1)
        self.refused("merge_base_mismatch", "merge-turn-land", "--turn", first, "--actor",
                     "task-alpha", "--landed-sha", self.a1, "--observed-base-sha", self.base,
                     "--evidence", "fast-forwarded main to A1")
        landed = self.ok("merge-turn-land", "--turn", first, "--actor", "task-alpha",
                         "--landed-sha", self.a1, "--evidence", "fast-forwarded main to A1")
        self.assertEqual(landed["released"]["observedBaseSha"], self.a1)

        # A store written by R3 holds the pre-landing base on that landing.
        with sqlite3.connect(os.path.join(self.state, "relay.sqlite3")) as db:
            db.execute("UPDATE merge_turns SET observed_base_sha = ? WHERE turn_id = ?",
                       (self.base, first))

        second = self.claim(self.a2)
        stale = self.refused("merge_currency_stale", *self.check(second, self.a2, self.a1))
        self.assertIn(first, stale["detail"])
        self.assertIn("merge-turn-restate-base", stale["detail"])

        restated = self.ok("merge-turn-restate-base", "--turn", first, "--actor", "task-alpha",
                           "--observed-base-sha", self.a1,
                           "--evidence", "main after A1's fast-forward")
        self.assertEqual(restated["observedBaseSha"], self.a1)
        self.assertEqual([(r["from"], r["to"], r["source"])
                          for r in restated["baseRestatements"]],
                         [(self.base, self.a1, LOCAL_GIT)])

        self.assertEqual(self.ok(*self.check(second, self.a2, self.a1))["state"], "merging")
        self.repo.point("main", self.a2)
        landed = self.ok("merge-turn-land", "--turn", second, "--actor", "task-alpha",
                         "--landed-sha", self.a2, "--observed-base-sha", self.a2,
                         "--evidence", "fast-forwarded main to A2")
        self.assertEqual(landed["released"]["state"], "landed")
        self.assertEqual(landed["released"]["observedBaseSha"], self.a2)

    def test_the_new_command_explains_its_arguments(self):
        buffer = io.StringIO()
        with redirect_stdout(buffer), self.assertRaises(SystemExit) as caught:
            cli.main(["merge-turn-restate-base", "--help"])
        self.assertEqual(caught.exception.code, 0)
        self.assertIn("--observed-base-sha", buffer.getvalue())
