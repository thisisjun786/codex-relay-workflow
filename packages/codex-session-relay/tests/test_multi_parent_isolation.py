"""Two parents, two repositories, two Linear projects, one store.

One shared service is the deployment this package is written for, and the existing
fairness and cross-assignment tests already cover a great deal of it: a busy parent not
spending the whole budget, a cancelled assignment leaving the others served, a completion
refused when it is addressed to somebody else's parent. Those are reused and named in
docs/contention-regression.md rather than repeated here.

What is left is the combination none of them holds. Two parents contending rather than
taking turns - the acknowledgement and the verdict paths driven from two threads with one
independent Store connection each, the way test_registry.py already contends a resume
against a generation advance. The barrier aligns the two starts and nothing more: the
scheduler may still serialise them, so what is asserted holds under every interleaving,
which is that each parent's acknowledgement, generation and revision name only its own
assignment. And a parent stuck at a BOUND rather than on one failure: a held attempt cap
and a spent hourly allowance are states no tick will retry, which is different from a send
that happens to fail again.

Also here because nothing else reads it: scope_ref, the Linear reference registration
accepts and no test has ever asserted survives.
"""

import os
import threading
import unittest

from codex_session_relay import identity
from codex_session_relay.ack import AckService
from codex_session_relay.delivery import DeliveryService
from codex_session_relay.errors import RelayError
from codex_session_relay.models import Endpoint
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.reconcile import Reconciler
from codex_session_relay.registry import Registry, project_key, record_settings
from codex_session_relay.store import Store
from codex_session_relay.sync import SyncOutbox
from codex_session_relay.delivery import REVISION
from codex_session_relay.transport import ACKNOWLEDGED, DISPATCHED

from .support import HOST, task_settings
from .test_daemon import DaemonTestCase

ALPHA_DOC = "https://linear.app/example/document/project-alpha-0000"
BETA_DOC = "https://linear.app/example/document/project-beta-00000"


class TwoParents(DaemonTestCase):
    """Parent a in one repository and Linear project, parent b in another."""

    PROJECTS = {
        "a": ("/repo-a", "AAA-1", "linear://project-alpha"),
        "b": ("/repo-b", "BBB-1", "linear://project-beta"),
    }

    def setUp(self):
        super().setUp()
        self.sync = SyncOutbox(self.store, self.clock)
        self.ack.sync = self.sync

    def parent_of(self, name):
        return f"01parent-{name}"

    def child_of(self, name):
        return f"01child-{name}"

    def assignment(self, name, *, events=1):
        cwd, issue, scope_ref = self.PROJECTS[name]
        parent, child = self.parent_of(name), self.child_of(name)
        root = os.path.join(self.root, name)
        os.makedirs(root, exist_ok=True)
        relationship = self.registry.register(
            parent=Endpoint(parent, HOST, cwd=cwd),
            child=Endpoint(child, HOST, cwd=root),
            issue_key=issue, artifact_roots=[root],
            # The child is an allowed recipient too, so a needs_changes verdict has somewhere
            # to route its revision; without it the isolation question never gets asked.
            allowed_recipients=[parent, child],
            dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=f"turn-{name}",
            scope_ref=scope_ref,
        )
        self.adapter.add_thread(parent)
        self.adapter.add_thread(child)
        record_settings(self.store, self.clock, parent, task_settings(cwd),
                        source="creation_result")
        ids = []
        for index in range(events):
            path = os.path.join(root, f"out-{index}.txt")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(f"{name}-{index}")
            payload = self.ready_payload(
                relationship, [path], attempt=index + 1,
                turn=self.assigned_turn(thread=child, turn=f"turn-{name}"),
            )
            self.accept(payload)
            self.delivery.enqueue(payload["eventId"])
            ids.append(payload["eventId"])
            self.clock.advance(1)
        return relationship, ids

    def deliver(self, event_id):
        return self.delivery.attempt(event_id, self.adapter, now=self.clock.now())

    def services_on_their_own_connection(self):
        """A second set of services over the same file, as another process would have.

        The caller closes it on the thread that opened it. sqlite3 binds a connection to its
        creating thread, so registering the close as test cleanup would run it on the main
        thread and raise there instead of here.
        """
        store = Store(self.store.path)
        registry = Registry(store, self.clock)
        intake = ReceiptIntake(store, registry, self.clock)
        delivery = DeliveryService(store, registry, intake, self.clock)
        ack = AckService(store, registry, intake, delivery, self.clock)
        ack.sync = SyncOutbox(store, self.clock)
        return store, ack

    def in_parallel(self, work):
        """Run two callables at one instant and report what each of them did.

        The barrier is the whole point: without it this proves the sequential case, which
        is already proved elsewhere.
        """
        barrier = threading.Barrier(len(work))
        results, errors = {}, {}

        def wrap(name, call):
            def run():
                store = None
                try:
                    store, service = self.services_on_their_own_connection()
                    barrier.wait(timeout=20)
                    results[name] = call(service)
                except Exception as error:  # noqa: BLE001 - asserted by the caller
                    errors[name] = error
                finally:
                    if store is not None:
                        store.close()

            return threading.Thread(target=run)

        threads = [wrap(name, call) for name, call in work.items()]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        for thread in threads:
            self.assertFalse(thread.is_alive(), "a thread never finished")
        return results, errors


class TwoParentsOnOneStore(TwoParents):
    def test_each_parent_keeps_the_scope_reference_it_registered(self):
        alpha, _ = self.assignment("a")
        beta, _ = self.assignment("b")

        rows = {
            row["relationship_id"]: row
            for row in self.store.all("SELECT * FROM relationships")
        }
        self.assertEqual(rows[alpha["relationshipId"]]["scope_ref"], "linear://project-alpha")
        self.assertEqual(rows[beta["relationshipId"]]["scope_ref"], "linear://project-beta")
        self.assertNotEqual(project_key(alpha), project_key(beta))
        self.assertEqual(project_key(alpha), "/repo-a")

    def test_two_parents_acknowledging_at_the_same_instant_do_not_cross(self):
        alpha, alpha_ids = self.assignment("a")
        beta, beta_ids = self.assignment("b")
        self.deliver(alpha_ids[0])
        self.deliver(beta_ids[0])
        self.clock.advance(5)
        turns = {
            name: self.adapter.start_turn(
                self.parent_of(name), turn_id=f"ack-{name}", status="inProgress",
            )
            for name in ("a", "b")
        }

        def acknowledge(name, event_id):
            def call(service):
                return service.acknowledge(
                    event_id, ack_turn_id=turns[name].turn_id,
                    ack_proof=identity.ack_proof(event_id, turns[name].turn_id),
                    accepted=True, adapter=self.adapter,
                )

            return call

        results, errors = self.in_parallel({
            "a": acknowledge("a", alpha_ids[0]),
            "b": acknowledge("b", beta_ids[0]),
        })

        self.assertEqual(errors, {}, f"neither acknowledgement may fail: {errors}")
        self.assertTrue(results["a"]["accepted"])
        self.assertTrue(results["b"]["accepted"])
        for name, ids, relationship in (
            ("a", alpha_ids, alpha), ("b", beta_ids, beta),
        ):
            row = self.store.one("SELECT * FROM acks WHERE event_id = ?", (ids[0],))
            self.assertIsNotNone(row, f"{name} has no acknowledgement")
            self.assertEqual(row["ack_turn_id"], f"ack-{name}")
            self.assertEqual(self.delivery.get(ids[0])["state"], ACKNOWLEDGED)
            self.assertEqual(
                self.store.one("SELECT * FROM events WHERE event_id = ?", (ids[0],))
                ["relationship_id"],
                relationship["relationshipId"],
            )

    def test_two_parents_recording_needs_changes_at_the_same_instant_open_one_generation_each(self):
        alpha, alpha_ids = self.assignment("a")
        beta, beta_ids = self.assignment("b")
        for name, ids in (("a", alpha_ids), ("b", beta_ids)):
            self.deliver(ids[0])
            self.clock.advance(5)
            turn = self.adapter.start_turn(
                self.parent_of(name), turn_id=f"ack-{name}", status="inProgress",
            )
            self.ack.acknowledge(
                ids[0], ack_turn_id=turn.turn_id,
                ack_proof=identity.ack_proof(ids[0], turn.turn_id), accepted=True,
                adapter=self.adapter,
            )

        def verdict(name, event_id):
            def call(service):
                return service.record_verdict(
                    event_id, verdict="needs_changes", verdict_turn_id=f"verdict-{name}",
                    findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix it"}],
                )

            return call

        results, errors = self.in_parallel({
            "a": verdict("a", alpha_ids[0]),
            "b": verdict("b", beta_ids[0]),
        })

        self.assertEqual(errors, {}, f"neither verdict may fail: {errors}")
        for name, relationship in (("a", alpha), ("b", beta)):
            rid = relationship["relationshipId"]
            self.assertEqual(results[name]["nextExecutionGeneration"], 2)
            self.assertEqual(self.registry.get(rid)["executionGeneration"], 2)
            revisions = self.store.all(
                "SELECT * FROM deliveries WHERE kind = ? AND relationship_id = ?",
                (REVISION, rid),
            )
            self.assertEqual(
                len(revisions), 1,
                f"{name} got {len(revisions)} revisions from one verdict",
            )
            self.assertEqual(
                revisions[0]["recipient_task_id"], self.child_of(name),
                "a revision was routed to the other project's child",
            )

    def test_each_outbox_job_names_its_own_document_and_another_jobs_claim_cannot_complete_it(self):
        jobs = {}
        for name, target in (("a", ALPHA_DOC), ("b", BETA_DOC)):
            relationship, ids = self.assignment(name)
            rid = relationship["relationshipId"]
            self.deliver(ids[0])
            self.clock.advance(5)
            turn = self.adapter.start_turn(
                self.parent_of(name), turn_id=f"ack-{name}", status="inProgress",
            )
            self.ack.acknowledge(
                ids[0], ack_turn_id=turn.turn_id,
                ack_proof=identity.ack_proof(ids[0], turn.turn_id), accepted=True,
                adapter=self.adapter,
            )
            self.sync.set_target(rid, "coordination_document", target)
            self.ack.record_verdict(
                ids[0], verdict="verified", verdict_turn_id=f"verdict-{name}",
            )
            jobs[name] = self.sync.snapshot(relationship_id=rid)["jobs"][0]["syncId"]

        self.assertNotEqual(
            jobs["a"], jobs["b"],
            "two projects' owed writes share one identity, so confirming either confirms both",
        )
        self.assertEqual(self.sync.get(jobs["a"])["target_ref"], ALPHA_DOC)
        self.assertEqual(self.sync.get(jobs["b"])["target_ref"], BETA_DOC)

        alpha_claim = self.sync.claim(jobs["a"], owner="worker-1", now=self.clock.now())
        self.sync.claim(jobs["b"], owner="worker-1", now=self.clock.now())
        with self.assertRaises(RelayError):
            self.sync.complete(
                jobs["b"], claim_token=alpha_claim["claimToken"], target_ref=BETA_DOC,
                readback="", now=self.clock.now(),
            )
        self.assertNotEqual(
            self.sync.get(jobs["b"])["state"], "confirmed",
            "one project's claim confirmed another project's write",
        )


class OneParentCappedTheOtherProgresses(TwoParents):
    """Capped is not the same as failing: no tick retries a held attempt cap."""

    def drain(self, ids, *, ticks=12):
        delivered = set()
        for _ in range(ticks):
            self.clock.advance(3600)
            self.daemon.tick(now=self.clock.now())
            delivered = {
                event_id for event_id in ids
                if self.delivery.get(event_id)["state"] == DISPATCHED
            }
            if delivered == set(ids):
                break
        return delivered

    def test_b_still_delivers_while_a_sits_at_its_attempt_cap(self):
        _alpha, alpha_ids = self.assignment("a")
        _beta, beta_ids = self.assignment("b", events=3)

        for _ in range(self.delivery.policy.max_attempts):
            self.adapter.script("read_fail")
            self.clock.advance(100000)
            self.deliver(alpha_ids[0])
        self.assertEqual(self.delivery.get(alpha_ids[0])["hold_reason"], "attempt_cap")
        attempts_before = len(self.attempts_for(alpha_ids[0]))

        self.assertEqual(
            self.drain(beta_ids), set(beta_ids),
            "the other project's work never drained while one project sat at its cap",
        )
        self.assertEqual(
            len(self.attempts_for(alpha_ids[0])), attempts_before,
            "a held attempt cap was retried, so the bound is not a bound",
        )

    def test_b_still_delivers_while_a_is_at_the_hourly_recipient_cap(self):
        _alpha, alpha_ids = self.assignment("a")
        _beta, beta_ids = self.assignment("b", events=3)
        now = self.clock.now()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO recipient_rate (recipient_task_id, window_start, sends,"
                " last_send_at) VALUES (?,?,?,?)",
                (
                    self.parent_of("a"), int(now // 3600) * 3600,
                    self.delivery.policy.max_sends_per_recipient_per_hour, now,
                ),
            )

        self.daemon.tick(now=now)

        self.assertNotIn(
            self.parent_of("a"), [thread for _r, thread, _m, _o in self.adapter.sends],
            "a recipient at its hourly cap was sent to anyway",
        )
        self.assertNotEqual(
            self.delivery.get(alpha_ids[0])["state"], DISPATCHED,
            "the capped recipient was delivered to inside the window it had already spent",
        )
        self.assertEqual(self.drain(beta_ids), set(beta_ids))
        # And the cap is a delay rather than a wall: drain() advances past the window, so the
        # parent that spent its allowance is served once a new hour starts. A bound that
        # stranded the work instead would be a different defect from the one being tested.
        self.assertEqual(
            self.delivery.get(alpha_ids[0])["state"], DISPATCHED,
            "an hourly allowance became a permanent refusal",
        )

    def test_pausing_then_archiving_a_stops_neither_b_nor_the_shared_daemon(self):
        alpha, alpha_ids = self.assignment("a")
        _beta, beta_ids = self.assignment("b", events=2)
        rid = alpha["relationshipId"]

        self.registry.set_status(rid, "paused", actor="the owner of project alpha")
        self.clock.advance(3600)
        first = self.daemon.tick(now=self.clock.now())
        self.assertFalse(first.quiet, "the shared daemon went quiet when one project paused")

        self.registry.set_status(rid, "archived", actor="the owner of project alpha")
        self.assertEqual(
            self.drain(beta_ids), set(beta_ids),
            "archiving one project stopped the other from being served",
        )
        self.assertNotEqual(
            self.delivery.get(alpha_ids[0])["state"], DISPATCHED,
            "a paused and then archived assignment was delivered anyway",
        )


if __name__ == "__main__":
    unittest.main()
