"""The coordination modules obey the contracts their own suite cannot state for them.

Two different things are checked here and they fail for different reasons.

The first is a SOURCE contract. This package's regression map enforces six inventories over
the whole tree, and every one of them declares its expected set inside
test_regression_map.py - a file the peer boundary for this work puts off limits. So a new
module that declares a bool-returning annotation, writes a name an existing folded boolean
already uses, or issues SQL whose head the transaction watcher cannot attribute, breaks the
build with no permitted repair. Finding that out from test_regression_map's own failure is
possible but slow and indirect; finding it out here names the file, the line and the rule.
The forbidden-name set is DERIVED from the three declared lists rather than copied, so this
file cannot drift from the one that enforces them.

The second is a BEHAVIOURAL contract, and it is the one criterion c6 rests on: no operation
here bundles a long repetition into a single wait. An injected clock cannot show that - a
FakeClock only moves when a caller advances it, so a method that polled in a real loop would
pass an unchanged-clock assertion. Counting transactions can: a mutator opens exactly one and
a reader opens none, which an internal retry or poll loop fails.
"""

import ast
import pathlib
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.linkage import Linkage, PARENT
from codex_session_relay.mergeturn import MergeTurn
from codex_session_relay.models import Endpoint
from codex_session_relay.store import Store

from .support import RelayTestCase, written_table
from .test_regression_map import (
    FOLD_FREE_BOOLEANS, FOLDS_BEYOND_ITS_PATHS, SUMMARIES,
)

SOURCE = pathlib.Path(__file__).resolve().parent.parent / "src" / "codex_session_relay"
# Every module this issue adds. A new one joins this tuple in the phase that creates it, so
# the contract reaches it the moment it exists rather than the first time CI complains.
OWNED = ("capacity.py", "coordination.py", "editregion.py", "mergeturn.py")

WAITING_NAMES = {"sleep", "monotonic", "perf_counter", "poll"}
WRITE_HEADS = ("INSERT", "REPLACE", "UPDATE", "DELETE", "WITH")


def folded_names():
    """The names a new module may not write, derived from the lists that enforce them.

    Copying the set would put a second copy of the rule in the tree, and the whole reason
    these inventories exist is that a second copy goes stale without failing anything.
    """
    keys = list(SUMMARIES) + list(FOLDS_BEYOND_ITS_PATHS) + list(FOLD_FREE_BOOLEANS)
    return {key[2] for key in keys}


def trees():
    for name in OWNED:
        path = SOURCE / name
        yield name, path, ast.parse(path.read_text(encoding="utf-8"))


def is_bool_annotation(node):
    return isinstance(node, ast.Name) and node.id == "bool"


class TheSourceObeysTheInventoriesItCannotDeclareItselfIn(unittest.TestCase):
    def test_no_module_waits_in_a_loop_of_its_own(self):
        """criterion c6, from the source side.

        A bounded walk over rows a query returned is not repetition bundled into a wait, and
        the promotion path is exactly that. An unbounded while is the shape that would be.
        """
        offenders = []
        for name, _path, tree in trees():
            for node in ast.walk(tree):
                if isinstance(node, ast.While):
                    offenders.append(name + ":" + str(node.lineno) + " while")
                if isinstance(node, ast.Name) and node.id in WAITING_NAMES:
                    offenders.append(name + ":" + str(node.lineno) + " " + node.id)
                if isinstance(node, ast.Attribute) and node.attr in WAITING_NAMES:
                    offenders.append(name + ":" + str(node.lineno) + " ." + node.attr)
        self.assertEqual(
            offenders, [],
            "these modules answer from one bounded read and never wait on anything, which is"
            " what lets a caller correct course between operations",
        )

    def test_no_module_declares_a_boolean_the_partition_would_have_to_receive(self):
        """Gate IV. The declaration keys on the ANNOTATION, so returning one is fine."""
        offenders = []
        for name, _path, tree in trees():
            for node in ast.walk(tree):
                if isinstance(node, ast.FunctionDef) and is_bool_annotation(node.returns):
                    offenders.append(name + ":" + str(node.lineno) + " -> bool " + node.name)
                if isinstance(node, ast.ClassDef):
                    for statement in node.body:
                        if isinstance(statement, ast.AnnAssign) and is_bool_annotation(
                                statement.annotation):
                            offenders.append(
                                name + ":" + str(statement.lineno) + " field "
                                + getattr(statement.target, "id", "?"))
        self.assertEqual(
            offenders, [],
            "a declared boolean has to be classified into one of three lists inside"
            " test_regression_map.py, which this work may not edit",
        )

    def test_no_module_writes_a_name_an_existing_folded_boolean_already_uses(self):
        """Gate II and gate IV's producer scan, which match on the bare name."""
        names = folded_names()
        offenders = []
        for name, _path, tree in trees():
            for node in ast.walk(tree):
                where = name + ":" + str(getattr(node, "lineno", 0))
                if isinstance(node, ast.FunctionDef) and node.name in names:
                    offenders.append(where + " def " + node.name)
                if isinstance(node, ast.Attribute) and node.attr in names:
                    offenders.append(where + " ." + node.attr)
                if isinstance(node, ast.Name) and node.id in names:
                    offenders.append(where + " " + node.id)
                if isinstance(node, ast.keyword) and node.arg in names:
                    offenders.append(where + " " + str(node.arg) + "=")
        self.assertEqual(
            offenders, [],
            "these names belong to booleans the regression map has already classified;"
            " reusing one changes what that map derives about a module we do not own",
        )

    def test_every_write_statement_is_one_the_transaction_watcher_can_attribute(self):
        """Gate V. A write behind a common table expression is the blind spot it watches."""
        unreadable = []
        for name, _path, tree in trees():
            for node in ast.walk(tree):
                if not (isinstance(node, ast.Constant) and isinstance(node.value, str)):
                    continue
                if not node.value.lstrip().startswith(WRITE_HEADS):
                    continue
                if written_table(node.value) is None:
                    unreadable.append(name + ":" + str(node.lineno) + " " + node.value[:48])
        self.assertEqual(
            unreadable, [],
            "a transaction holding this statement would look emptier than it is",
        )

    def test_no_module_declares_the_lock_wait_a_second_time(self):
        """Gate VI, which lives outside the regression map entirely."""
        offenders = []
        for name, _path, tree in trees():
            for node in ast.walk(tree):
                targets = []
                if isinstance(node, ast.Assign):
                    targets = node.targets
                elif isinstance(node, ast.AnnAssign):
                    targets = [node.target]
                for target in targets:
                    if isinstance(target, ast.Name) and target.id == "SQLITE_TIMEOUT":
                        offenders.append(name + ":" + str(node.lineno))
        self.assertEqual(
            offenders, [], "the bound is declared once, in intent.py, and these open no"
            " connection of their own")

class OneBoundedWriteAndThenAnAnswer(RelayTestCase):
    """criterion c6, from the behaviour side.

    An unchanged FakeClock proves nothing here, because a FakeClock only moves when a caller
    advances it: a method that polled in a real loop would pass that assertion untouched.
    Counting transactions is an oracle such a method fails. A mutator opens exactly one and
    then answers; a reader opens none. Neither leaves a caller waiting on a cell that is
    quietly repeating something.
    """

    PROJECT_KEY = "PRJ-A"
    GREEN = {"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
             "threadsSeen": ["thread-1"], "unresolved": 0}

    def setUp(self):
        super().setUp()
        self.linkage = Linkage(self.store, self.clock)
        self.turns = MergeTurn(self.store, self.clock, self.linkage)
        self.alpha = Endpoint("task-alpha", "host-a", cwd="/alpha")
        self.linkage.bind_scope(
            role=PARENT, scope_key=self.PROJECT_KEY, endpoint=self.alpha)

    def transactions_during(self, call):
        """How many transactions one call opens, counted by shimming the store's own opener."""
        original = self.store.transaction
        tally = {"opened": 0}

        def shim():
            tally["opened"] += 1
            return original()

        self.store.transaction = shim
        try:
            call()
        finally:
            self.store.transaction = original
        return tally["opened"]

    def claim(self, head="head-a"):
        return self.turns.request(
            repository="owner/repo", base_ref="dev", project_key=self.PROJECT_KEY,
            holder=self.alpha, candidate_head=head, ready=True)

    def test_every_mutator_opens_exactly_one_transaction(self):
        held = self.claim()
        identifier = held["turnId"]
        self.turns.acknowledge_grant(
            identifier, actor=self.alpha.task_id,
            grant=self.turns.turn(identifier)["grant"]["grantId"],
            evidence="read the grant and re-checked the record")
        opened = {
            "declare_ready": self.transactions_during(
                lambda: self.turns.declare_ready(
                    identifier, actor=self.alpha.task_id, ready=True)),
            "attest": self.transactions_during(
                lambda: self.turns.attest(
                    identifier, evidence_kind="transport_accepted",
                    idempotency_key="delivery-1", actor="task-beta", evidence="accepted")),
            "begin_merge": self.transactions_during(
                lambda: self.turns.begin_merge(
                    identifier, actor=self.alpha.task_id, head_sha="head-a",
                    base_sha="base-0", required=["dev-gate"],
                    checks=[{"runId": "run-1", "name": "dev-gate", "headSha": "head-a",
                             "conclusion": "success", "attempt": 1}],
                    review=dict(self.GREEN))),
            "land": self.transactions_during(
                lambda: self.turns.land(
                    identifier, actor=self.alpha.task_id, landed_sha="merge-1",
                    observed_base_sha="base-1", evidence="the merge commit is on the base")),
        }
        self.assertEqual(
            opened, {"declare_ready": 1, "attest": 1, "begin_merge": 1, "land": 1},
            "a mutator that opened a second transaction would be doing two things a caller"
            " cannot correct between",
        )

    def test_a_claim_opens_exactly_one_transaction(self):
        self.assertEqual(self.transactions_during(self.claim), 1)

    def test_a_refused_mutator_still_opens_exactly_one(self):
        held = self.claim()
        self.turns.acknowledge_grant(
            held["turnId"], actor=self.alpha.task_id,
            grant=self.turns.turn(held["turnId"])["grant"]["grantId"],
            evidence="read the grant and re-checked the record")

        def refused():
            try:
                self.turns.begin_merge(
                    held["turnId"], actor=self.alpha.task_id, head_sha="head-moved",
                    base_sha="base-0", checks=[], review=dict(self.GREEN))
            except Exception:  # noqa: BLE001 - the refusal is the point, not its type
                pass

        self.assertEqual(self.transactions_during(refused), 1)

    def test_every_reader_opens_none(self):
        held = self.claim()
        identifier = held["turnId"]
        opened = {
            "turn": self.transactions_during(lambda: self.turns.turn(identifier)),
            "ledger": self.transactions_during(lambda: self.turns.ledger(identifier)),
            "target": self.transactions_during(
                lambda: self.turns.target("owner/repo", "dev")),
        }
        self.assertEqual(
            opened, {"turn": 0, "ledger": 0, "target": 0},
            "reading who holds a target takes no write lock, so a reader never blocks the"
            " holder it is reading about",
        )

    def test_reading_the_target_twice_answers_the_same_without_the_clock_moving(self):
        self.claim()
        before = self.turns.target("owner/repo", "dev")
        self.clock.advance(1_000_000)
        self.assertEqual(self.turns.target("owner/repo", "dev"), before)


if __name__ == "__main__":
    unittest.main()
