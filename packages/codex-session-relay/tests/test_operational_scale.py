"""How far a bounded daemon carries an assignment, and how much of one it carries at once.

Two halves, and they answer different questions with different clocks.

The lifetime half extends the crossing CRW-7 built in test_service.py. That harness drives
the supervisor past four hours of scripted monotonic time and already asserts the store
identity and the generations survive every worker replacement; what it does not reach is
the rest of the handoff. Here the store carries a dispatched delivery, its acknowledgement
and an owed coordination write before the crossing starts, and all three are compared
afterwards. The limit that test wrote down applies unchanged and is repeated here because
it is easy to lose: a FakeWorker exits in microseconds and never opens the store, so this
shows the SUPERVISOR preserving those rows. Whether a replacement worker can read them
back is a different question, and ARealWorkerReadsTheHandoffBack is the only thing here
that answers it - in a real process, in real time, for one short segment.

The scale half declares its load as module constants so a report quotes a number read from
source. What it establishes: at this size, selection stays inside the ceilings the policy
declares, the backlog only shrinks, and no parent is served twice before every parent is
served once. What it does not establish: anything about throughput, about a host under
real load, or about a size larger than the one written below. Passing here is not support
for unbounded parallel operation.
"""

import json
import math
import os
import unittest
from pathlib import Path

from codex_session_relay import identity
from codex_session_relay.ack import AckService
from codex_session_relay.clock import FakeClock
from codex_session_relay.daemon import RelayDaemon, SingleInstance
from codex_session_relay.delivery import DeliveryService
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.models import Endpoint
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.reconcile import Reconciler
from codex_session_relay.registry import Registry, record_settings
from codex_session_relay.store import Store
from codex_session_relay.sync import SyncOutbox
from codex_session_relay.transport import DISPATCHED

from .support import HOST, task_settings
from .test_daemon import DaemonTestCase
from .test_service import FOUR_HOURS, SOCKET, FakeWorker, ScriptedClock, ServiceTestCase

# The declared load. Read from here by the tests and by any report that quotes a number.
PARENTS = 6
EVENTS_PER_PARENT = 12
TOTAL_EVENTS = PARENTS * EVENTS_PER_PARENT

CROSSING_DOC = "https://linear.app/example/document/crossing-0000000000"


class BoundedRunsResumeWithoutDuplicating(DaemonTestCase):
    """run() is bounded by construction, so finishing anything takes more than one of them."""

    def reopen(self):
        path = self.store.path
        self.store.close()
        self.store = Store(path)
        self.addCleanup(self.store.close)
        self.registry = Registry(self.store, self.clock)
        self.intake = ReceiptIntake(self.store, self.registry, self.clock)
        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.ack = AckService(self.store, self.registry, self.intake, self.delivery, self.clock)
        self.reconciler = Reconciler(self.store, self.registry, self.delivery, self.clock)
        self.daemon = RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, self.adapter, clock=self.clock,
        )

    def test_work_spanning_several_tick_limited_runs_is_delivered_exactly_once(self):
        _relationship, event_id = self.queued_event()

        for _ in range(3):
            self.clock.advance(3600)
            self.daemon.run(max_ticks=1)
            self.reopen()

        self.assertEqual(
            len(self.adapter.sends), 1,
            f"three bounded runs over one handoff sent it {len(self.adapter.sends)} times",
        )
        self.assertEqual(self.delivery.get(event_id)["state"], DISPATCHED)
        self.assertEqual(len(self.attempts_for(event_id)), 1)

    def test_a_second_daemon_refuses_while_the_first_still_owes_work_and_the_first_completes_it(self):
        """The refusal itself is covered in test_daemon.py; this is what it costs the work."""
        _relationship, event_id = self.queued_event()

        with SingleInstance(self.tmp):
            with self.assertRaises(RuntimeError):
                with SingleInstance(self.tmp):
                    pass
            self.assertEqual(
                self.delivery.get(event_id)["state"], "queued",
                "the contested start consumed the handoff it was refused from taking",
            )

        self.clock.advance(3600)
        self.daemon.run(max_ticks=1)

        self.assertEqual(self.delivery.get(event_id)["state"], DISPATCHED)
        self.assertEqual(
            len(self.adapter.sends), 1, "the refused second daemon left a duplicate behind",
        )


class CrossingFixture(ServiceTestCase):
    """A service whose store already holds a whole handoff, ready to be carried across."""

    def handoff(self, service):
        """Register, deliver, acknowledge and owe a coordination write, all in one store."""
        store = Store(Path(service.selection.path) / "relay.sqlite3")
        self.addCleanup(store.close)
        clock = FakeClock()
        registry = Registry(store, clock)
        intake = ReceiptIntake(store, registry, clock)
        delivery = DeliveryService(store, registry, intake, clock)
        sync = SyncOutbox(store, clock)
        ack = AckService(store, registry, intake, delivery, clock)
        ack.sync = sync
        adapter = FakeHostAdapter(clock)

        parent, child = "01parent-task", "01child-task"
        root = os.path.join(self.tmp, "work")
        os.makedirs(root, exist_ok=True)
        relationship = registry.register(
            parent=Endpoint(parent, HOST, cwd="/parent"),
            child=Endpoint(child, HOST, cwd=root),
            issue_key="REL-9", artifact_roots=[root],
            allowed_recipients=[parent, child],
            dispatch_request_id="dispatch-9", dispatch_turn_id="turn-dispatch-9",
        )
        rid = relationship["relationshipId"]
        record_settings(store, clock, parent, task_settings("/parent"), source="creation_result")
        adapter.add_thread(parent)
        adapter.add_thread(child)

        path = os.path.join(root, "out.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("the deliverable")
        from codex_session_relay import manifest

        entries, _bindings = manifest.build([path], relationship["authorizedScope"]["artifactRoots"])
        digest = manifest.revision_hash(entries)
        from codex_session_relay.models import TurnRef

        turn = TurnRef(child, "turn-dispatch-9", "completed")
        payload = {
            "eventId": identity.event_id(
                rid, 1, digest, "ready_for_review", turn_id=turn.turn_id, attempt=1,
            ),
            "relationshipId": rid, "executionGeneration": 1, "attempt": 1,
            "revisionHash": digest, "outcome": "ready_for_review", "producer": "child",
            "turnRef": turn.to_record(), "manifest": [e.to_record() for e in entries],
            "emittedAt": clock.iso(),
        }
        intake.accept_child_receipt(payload, observation=turn)
        event_id = payload["eventId"]
        delivery.enqueue(event_id)
        delivery.attempt(event_id, adapter, now=clock.now())
        clock.advance(5)
        ack_turn = adapter.start_turn(parent, turn_id="ack-turn", status="inProgress")
        ack.acknowledge(
            event_id, ack_turn_id=ack_turn.turn_id,
            ack_proof=identity.ack_proof(event_id, ack_turn.turn_id), accepted=True,
            adapter=adapter,
        )
        sync.set_target(rid, "coordination_document", CROSSING_DOC)
        ack.record_verdict(event_id, verdict="verified", verdict_turn_id="verdict-9")
        return store, rid, event_id

    def snapshot(self, store, rid, event_id):
        return {
            "delivery": dict(store.one(
                "SELECT * FROM deliveries WHERE event_id = ?", (event_id,))),
            "ack": dict(store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))),
            "outbox": [dict(row) for row in store.all(
                "SELECT * FROM sync_outbox WHERE relationship_id = ? ORDER BY sync_id", (rid,))],
            "generations": [dict(row) for row in store.all(
                "SELECT * FROM generations WHERE relationship_id = ?"
                " ORDER BY execution_generation", (rid,))],
        }


class TheCrossingPreservesWhatTheSupervisorHolds(CrossingFixture):
    """Past four hours of scripted time, with the whole handoff in the store.

    WHAT THIS PROVES: the supervisor replaces bounded workers past the boundary without the
    dispatched delivery, its acknowledgement, its owed coordination write or its generations
    changing, and without the store identity moving.

    WHAT IT DOES NOT: anything about four real hours, and anything about a worker. The
    FakeWorker below exits immediately and never opens this store, exactly as CRW-7 recorded
    when it wrote the same caveat for the generations. ARealWorkerReadsTheHandoffBack is
    where a real process reads the handoff back.
    """

    def test_a_dispatched_delivery_its_acknowledgement_and_its_outbox_job_are_unchanged(self):
        service = self.service("a")
        service.enable(actor="test")
        store, rid, event_id = self.handoff(service)
        before = self.snapshot(store, rid, event_id)
        before_id = service.store_id
        self.assertEqual(before["delivery"]["state"], "acknowledged")
        self.assertTrue(before["outbox"], "the fixture owed no coordination write to carry")

        clock = ScriptedClock()
        launches = []

        def spawn(**call):
            launches.append(call)
            clock.advance(call["segment_seconds"])
            return FakeWorker(0)

        outcome = service.supervise(
            allow_isolated=True, spawn=spawn, sleeper=clock.advance,
            segment_seconds=30 * 60, max_segments=9, monotonic=clock,
        )

        self.assertGreater(clock.elapsed, FOUR_HOURS)
        self.assertEqual(outcome["segments"], [0] * 9)
        self.assertEqual(len(launches), 9)
        self.assertEqual(service.store_id, before_id)
        self.assertEqual(
            self.snapshot(store, rid, event_id), before,
            "the handoff did not survive the crossing intact",
        )
        self.assertEqual(len({call["token"] for call in launches}), 1)


class ARealWorkerReadsTheHandoffBack(CrossingFixture):
    """The boundary a scripted clock cannot cross, for the delivery rather than the turn.

    Real time, one short segment, and a socket path this test owns which does not exist - so
    the worker's only way to name this handoff is to have opened the inherited store.
    """

    def test_a_real_replacement_worker_records_against_the_handoff_it_found(self):
        socket = os.path.join(self.tmp, "absent-app-server.sock")
        service = self.service("a", socket=socket)
        service.enable(actor="test")
        store, rid, event_id = self.handoff(service)
        # A second handoff, left queued, so the worker has something it must try to send.
        queued = self.queue_a_second_event(store, rid)
        store.close()

        self.assertFalse(os.path.exists(socket), "the worker would have reached a real endpoint")

        spawned = []

        def spawn(**call):
            child = service.spawn_worker(**call)
            spawned.append(child.pid)
            return child

        outcome = service.supervise(
            allow_isolated=True, segment_seconds=1.0, max_segments=1, spawn=spawn,
        )

        log = (service.selection.path / "daemon.log").read_text(encoding="utf-8")
        self.assertEqual(outcome["segments"], [0], f"the worker did not exit cleanly:\n{log}")
        self.assertEqual(len(spawned), 1)
        self.assertNotEqual(spawned[0], os.getpid(), "that was not a separate process")

        reopened = Store(Path(service.selection.path) / "relay.sqlite3")
        self.addCleanup(reopened.close)
        written = reopened.all(
            "SELECT * FROM failed_operations WHERE relationship_id = ?", (rid,),
        )
        report = json.loads(log)
        self.assertTrue(
            written,
            "the worker wrote nothing against this relationship, so nothing here shows it"
            f" opened the inherited store: {report}",
        )
        self.assertEqual(
            reopened.one("SELECT * FROM deliveries WHERE event_id = ?", (queued,))["state"],
            "queued",
            "a worker with no reachable host still marked the handoff as sent",
        )

    def queue_a_second_event(self, store, rid):
        from codex_session_relay import manifest
        from codex_session_relay.models import TurnRef

        clock = FakeClock()
        registry = Registry(store, clock)
        intake = ReceiptIntake(store, registry, clock)
        delivery = DeliveryService(store, registry, intake, clock)
        relationship = registry.get(rid)
        root = relationship["authorizedScope"]["artifactRoots"][0]
        path = os.path.join(root, "second.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("still owed")
        entries, _bindings = manifest.build([path], relationship["authorizedScope"]["artifactRoots"])
        digest = manifest.revision_hash(entries)
        turn = TurnRef("01child-task", "turn-dispatch-9", "completed")
        payload = {
            "eventId": identity.event_id(
                rid, 1, digest, "ready_for_review", turn_id=turn.turn_id, attempt=2,
            ),
            "relationshipId": rid, "executionGeneration": 1, "attempt": 2,
            "revisionHash": digest, "outcome": "ready_for_review", "producer": "child",
            "turnRef": turn.to_record(), "manifest": [e.to_record() for e in entries],
            "emittedAt": clock.iso(),
        }
        intake.accept_child_receipt(payload, observation=turn)
        delivery.enqueue(payload["eventId"])
        return payload["eventId"]


class TheDeclaredLoad(DaemonTestCase):
    """PARENTS parents, EVENTS_PER_PARENT events each, measured against the policy's own ceilings."""

    def load(self):
        ids = {}
        for index in range(PARENTS):
            name = f"p{index}"
            parent, child = f"01parent-{name}", f"01child-{name}"
            root = os.path.join(self.root, name)
            os.makedirs(root, exist_ok=True)
            relationship = self.registry.register(
                parent=Endpoint(parent, HOST, cwd=f"/repo/{name}"),
                child=Endpoint(child, HOST, cwd=root),
                issue_key=f"REL-{name}", artifact_roots=[root],
                allowed_recipients=[parent],
                dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=f"turn-{name}",
            )
            self.adapter.add_thread(parent)
            self.adapter.add_thread(child)
            record_settings(self.store, self.clock, parent, task_settings(f"/repo/{name}"),
                            source="creation_result")
            ids[parent] = []
            for event in range(EVENTS_PER_PARENT):
                path = os.path.join(root, f"out-{event}.txt")
                with open(path, "w", encoding="utf-8") as handle:
                    handle.write(f"{name}-{event}")
                payload = self.ready_payload(
                    relationship, [path], attempt=event + 1,
                    turn=self.assigned_turn(thread=child, turn=f"turn-{name}"),
                )
                self.accept(payload)
                self.delivery.enqueue(payload["eventId"])
                ids[parent].append(payload["eventId"])
                self.clock.advance(1)
        return ids

    def drain(self, *, limit):
        """Tick until nothing is queued, recording what each tick sent and what was left."""
        history = []
        seen = 0
        for _ in range(limit):
            self.clock.advance(3600)
            self.daemon.tick(now=self.clock.now())
            sent = [thread for _r, thread, _m, _o in self.adapter.sends[seen:]]
            seen = len(self.adapter.sends)
            backlog = self.store.one(
                "SELECT COUNT(*) AS c FROM deliveries WHERE state = 'queued'")["c"]
            history.append((sent, backlog))
            if backlog == 0:
                break
        return history

    def test_the_declared_load_drains_within_a_ceiling_derived_from_the_policy(self):
        self.load()
        policy = self.delivery.policy
        # Derived, never written down: the fewest ticks the per-tick send ceiling allows, with
        # room for the rotation to reach every parent rather than the fullest one.
        floor = math.ceil(TOTAL_EVENTS / policy.max_sends_per_tick)
        ceiling = floor * 2

        history = self.drain(limit=ceiling + 5)

        self.assertEqual(
            history[-1][1], 0,
            f"{TOTAL_EVENTS} events over {PARENTS} parents did not drain in {len(history)}"
            f" ticks; {history[-1][1]} left",
        )
        self.assertLessEqual(
            len(history), ceiling,
            f"draining took {len(history)} ticks against a derived ceiling of {ceiling}",
        )
        self.assertGreaterEqual(len(history), floor, "the per-tick ceiling was not respected")

    def test_no_tick_exceeds_the_per_tick_or_per_parent_send_ceiling(self):
        self.load()
        policy = self.delivery.policy

        for index, (sent, _backlog) in enumerate(self.drain(limit=80)):
            self.assertLessEqual(
                len(sent), policy.max_sends_per_tick, f"tick {index} sent {len(sent)}",
            )
            for parent in set(sent):
                self.assertLessEqual(
                    sent.count(parent), policy.max_sends_per_parent_per_tick,
                    f"tick {index} sent {sent.count(parent)} to {parent}",
                )

    def test_the_backlog_only_shrinks_while_the_daemon_is_running(self):
        self.load()
        history = self.drain(limit=80)
        backlogs = [backlog for _sent, backlog in history]
        self.assertEqual(
            backlogs, sorted(backlogs, reverse=True),
            f"the queue grew while the daemon was draining it: {backlogs}",
        )
        self.assertEqual(backlogs[0], TOTAL_EVENTS - len(history[0][0]))

    def test_no_parent_is_served_twice_over_before_every_parent_is_served_once(self):
        self.load()
        history = self.drain(limit=80)
        served = {}
        first_round = None
        for sent, _backlog in history:
            for parent in sent:
                served[parent] = served.get(parent, 0) + 1
            if first_round is None and len(served) == PARENTS:
                first_round = max(served.values())
        self.assertIsNotNone(first_round, f"not every parent was served: {served}")
        self.assertLessEqual(
            first_round, EVENTS_PER_PARENT,
            f"one parent was drained before another was reached at all: {served}",
        )


if __name__ == "__main__":
    unittest.main()

