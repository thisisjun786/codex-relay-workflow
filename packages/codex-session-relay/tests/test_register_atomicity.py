"""One registration is one commit, or it is nothing.

cmd_register holds two halves of the same fact: the relationship the parent and child are
about to work under, and the execution settings a later send has to preserve. CRW-127 moved
every check that could refuse the pair in front of both writes, so a contradiction the
arguments already carried stopped committing half the registration. What it could not move
was the failure nobody validates for -- the worker dying, the disk refusing, COMMIT itself
failing -- because those arrive between the two writes rather than before them.

These cases drive the seam with the fault the store already models: killed_before_commit
fires just before a COMMIT, so a registration interrupted there is a registration that was
half durable. Naming the table is what makes each case reach the interval it is named for.

The load-bearing assertion in each is not only that the store is empty afterwards. It is
that the interrupted transaction held BOTH tables, because an empty store also describes a
registration that never started, and these cases are about one that did.
"""

import argparse
import json
import types

from codex_session_relay.cli import cmd_register
from codex_session_relay.errors import RelayError
from codex_session_relay.models import Endpoint

from .support import CHILD, HOST, ISSUE, PARENT, RelayTestCase, WorkerKilled
from .support import killed_before_commit, task_settings


class RegisterAtomicity(RelayTestCase):
    def services(self):
        """Only what cmd_register reads, so the command surface is exercised, not rebuilt."""
        return types.SimpleNamespace(
            registry=self.registry, store=self.store, clock=self.clock,
        )

    def args(self, **overrides):
        settings = json.dumps(task_settings(self.root))
        namespace = dict(
            parent_task=PARENT, parent_host=HOST, parent_cwd="/parent",
            parent_cxc_session="cxc-parent",
            child_task=CHILD, child_host=HOST, child_cwd=self.root,
            child_cxc_session="cxc-child",
            issue=ISSUE, artifact_root=[self.root], allowed_recipient=[PARENT],
            scope_ref=None, dispatch_request_id="dispatch-1", dispatch_turn_id=None,
            supersedes=None, project=None,
            parent_settings=settings, parent_role=None, parent_exception=None,
            child_settings=settings, child_role=None, child_exception=None,
        )
        namespace.update(overrides)
        return argparse.Namespace(**namespace)

    def counts(self):
        return {
            "relationships": self.store.one(
                "SELECT COUNT(*) AS c FROM relationships")["c"],
            "generations": self.store.one(
                "SELECT COUNT(*) AS c FROM generations")["c"],
            "authorized_settings": self.store.one(
                "SELECT COUNT(*) AS c FROM authorized_settings")["c"],
        }

    # ------------------------------------------------------------------ the two intervals

    def test_killed_before_the_settings_commit_leaves_no_relationship_behind(self):
        """The interval CRW-173 is about: the relationship was written, the settings were not.

        Before the fix the relationship had its own committed transaction, so this kill left a
        live assignment for a task whose authorized settings nobody had recorded -- a send
        against it would refuse for a reason that names the settings and says nothing about
        the registration that is actually half-written.
        """
        with self.assertRaises(WorkerKilled):
            with killed_before_commit(self.store, writing="authorized_settings") as kill:
                cmd_register(self.services(), self.args())
        self.assertIn(
            "relationships", kill.killed,
            "the relationship and the settings are not written in one transaction, so a"
            f" registration interrupted here is half durable: {kill.killed}",
        )
        self.assertEqual(
            self.counts(), {"relationships": 0, "generations": 0, "authorized_settings": 0},
        )


class ContestOutlivesTheComposedRollback(RegisterAtomicity):
    """Composing the writes must not cost the evidence a refusal is required to leave.

    Three sites record a linkage contest and then raise, and they were right to expect their
    own transaction to commit it. Under one composed transaction that stops being true: the
    refusal rolls back the registration it was raised from, and the contest is inside it.
    linkage.py says what is owed here -- carrying the refusal on the error lets the caller
    write the contest once the rollback is over -- and the caller is now this command.
    """

    def contested(self):
        """A project whose parent role is already held by somebody else."""
        self.registry.linkage.bind_scope(
            role="parent", scope_key="PROJ-1",
            endpoint=Endpoint("01parent-two", HOST, cwd="/other", cxc_session="cxc-other"),
        )

    def conflicts(self):
        return self.store.all("SELECT * FROM linkage_conflicts ORDER BY id")

    def test_the_contest_is_readable_after_the_registration_rolls_back(self):
        self.contested()
        with self.assertRaises(RelayError):
            cmd_register(self.services(), self.args(project="PROJ-1"))
        self.assertEqual(
            self.counts(), {"relationships": 0, "generations": 0, "authorized_settings": 0},
            "a refused registration left part of itself behind",
        )
        self.assertEqual(
            len(self.conflicts()), 1,
            "the contest went down with the transaction the refusal rolled back, so the"
            " refusal reported itself and left no evidence of what it lost to",
        )

    def test_the_same_refusal_repeated_still_records_one_contest(self):
        """The composed path must not write the contest twice, here or through a retry."""
        self.contested()
        for _ in range(2):
            with self.assertRaises(RelayError):
                cmd_register(self.services(), self.args(project="PROJ-1"))
        self.assertEqual(len(self.conflicts()), 1)
        refused = [
            row for row in self.store.all("SELECT kind FROM journal")
            if row["kind"] == "linkage_refused"
        ]
        self.assertEqual(
            len(refused), 2,
            "one journal line per refusal: a doubled line means the contest was written both"
            f" inside the rolled-back transaction and again afterwards. Saw {len(refused)}",
        )

    def test_killed_before_the_relationship_commit_leaves_no_settings_behind(self):
        """The same seam from the other side, so neither order can pass this file alone."""
        with self.assertRaises(WorkerKilled):
            with killed_before_commit(self.store, writing="relationships") as kill:
                cmd_register(self.services(), self.args())
        self.assertIn(
            "authorized_settings", kill.killed,
            "the settings are written outside the transaction that writes the relationship,"
            f" so the two can still disagree after a failure: {kill.killed}",
        )
        self.assertEqual(
            self.counts(), {"relationships": 0, "generations": 0, "authorized_settings": 0},
        )
