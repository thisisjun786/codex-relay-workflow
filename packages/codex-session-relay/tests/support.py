"""Shared fixtures. Every test runs in its own temporary tree with a clock it controls.

Fixture identities match the REGISTRY on purpose: the child thread is the registered child
task id and the turn is the bound dispatch turn. A fixture that quietly used a foreign
thread or an unassigned turn would bless exactly the identity confusion the intake checks
are there to catch.
"""

import json
import os
import shutil
import tempfile
import unittest

from codex_session_relay import NO_DELIVERABLE, identity, manifest
from codex_session_relay.clock import FakeClock
from codex_session_relay.models import Endpoint, TurnRef
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.registry import Registry
from codex_session_relay.registry import record_settings
from codex_session_relay.store import Store

PARENT = "01parent-task"
CHILD = "01child-task"
ISSUE = "REL-1"
HOST = "host-a"
DISPATCH_TURN = "turn-dispatch-1"

_UNSET = object()


def task_settings(cwd, **overrides):
    """The execution settings a creation result reports, shaped like the real receipt."""
    settings = {
        "sandbox": {"type": "workspaceWrite", "writableRoots": [], "networkAccess": False,
                    "excludeTmpdirEnvVar": False, "excludeSlashTmp": False},
        "approvalPolicy": "never",
        "cwd": cwd,
        "runtimeWorkspaceRoots": [cwd],
        "model": "anthropic/claude-opus-5",
        "reasoningEffort": "xhigh",
        "environments": [
            {"environmentId": "local", "cwd": cwd, "runtimeWorkspaceRoots": [cwd]},
        ],
    }
    settings.update(overrides)
    return settings


class RelayTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-test-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.root = os.path.join(self.tmp, "work")
        os.makedirs(self.root)
        self.clock = FakeClock()
        self.store = Store(os.path.join(self.tmp, "state", "relay.sqlite3"))
        self.addCleanup(self.store.close)
        self.registry = Registry(self.store, self.clock)
        self.intake = ReceiptIntake(self.store, self.registry, self.clock)

    # ----------------------------------------------------------- fixtures

    def artifact(self, name: str, text: str) -> str:
        path = os.path.join(self.root, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(text)
        return path

    def register(self, *, roots=None, recipients=None, dispatch_turn_id=DISPATCH_TURN, **kw):
        settings = kw.pop("settings", _UNSET)
        relationship = self.registry.register(
            parent=Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent"),
            child=Endpoint(CHILD, HOST, cwd=self.root, cxc_session="cxc-child"),
            issue_key=kw.pop("issue_key", ISSUE),
            artifact_roots=roots if roots is not None else [self.root],
            allowed_recipients=recipients if recipients is not None else [PARENT],
            dispatch_request_id=kw.pop("dispatch_request_id", "dispatch-1"),
            dispatch_turn_id=dispatch_turn_id,
            **kw,
        )
        # Registration is where the creation result's execution settings are recorded, so the
        # ordinary fixture has them and every delivery test exercises the real settings path.
        # Pass settings=None to a test that wants the unrecorded case.
        if settings is not _UNSET:
            if settings is not None:
                # Written raw, past the validating recorder, so a test can stage the record a
                # stricter registration would have refused: an older writer, or a hand edit.
                # Delivery re-validates at send time, which is what these tests exercise.
                self.store.db.execute(
                    "INSERT INTO authorized_settings (task_id, settings, source, recorded_at)"
                    " VALUES (?,?,?,?)",
                    (PARENT, json.dumps(settings), "test-raw", self.clock.iso()),
                )
        else:
            record_settings(self.store, self.clock, PARENT, task_settings("/parent"),
                            source="creation_result")
            record_settings(self.store, self.clock, CHILD, task_settings(self.root),
                            source="creation_result")
        return relationship

    @staticmethod
    def assigned_turn(status="completed", *, thread=CHILD, turn=DISPATCH_TURN) -> TurnRef:
        return TurnRef(thread, turn, status)

    def ready_payload(self, relationship, paths, *, attempt=1, generation=None, turn=None):
        """A well-formed reviewable receipt over real bytes, as the child would emit it."""
        rid = relationship["relationshipId"]
        generation = generation or relationship["executionGeneration"]
        entries, _bindings = manifest.build(paths, relationship["authorizedScope"]["artifactRoots"])
        digest = manifest.revision_hash(entries)
        turn = turn or self.assigned_turn()
        return {
            "eventId": identity.event_id(
                rid, generation, digest, "ready_for_review",
                turn_id=turn.turn_id, attempt=attempt,
            ),
            "relationshipId": rid,
            "executionGeneration": generation,
            "attempt": attempt,
            "revisionHash": digest,
            "outcome": "ready_for_review",
            "producer": "child",
            "turnRef": turn.to_record(),
            "manifest": [e.to_record() for e in entries],
            "emittedAt": self.clock.iso(),
        }

    def execution_payload(self, relationship, outcome, *, attempt=1, turn=None, generation=None):
        rid = relationship["relationshipId"]
        generation = generation or relationship["executionGeneration"]
        status = {"failed": "failed", "interrupted": "interrupted"}.get(outcome, "completed")
        turn = turn or self.assigned_turn(status)
        return {
            "eventId": identity.event_id(
                rid, generation, NO_DELIVERABLE, outcome,
                turn_id=turn.turn_id, attempt=attempt,
            ),
            "relationshipId": rid,
            "executionGeneration": generation,
            "attempt": attempt,
            "revisionHash": NO_DELIVERABLE,
            "outcome": outcome,
            "producer": "child",
            "turnRef": turn.to_record(),
            "manifest": None,
            "emittedAt": self.clock.iso(),
        }

    def accept(self, payload, *, observation=None, intake=None):
        """Accept with the observation the payload names, unless a test supplies another."""
        if observation is None:
            observation = TurnRef(**{
                "thread_id": payload["turnRef"]["threadId"],
                "turn_id": payload["turnRef"]["turnId"],
                "turn_status": payload["turnRef"]["turnStatus"],
            })
        return (intake or self.intake).accept_child_receipt(payload, observation=observation)

    def assertRefused(self, reason, callable_, *args, **kwargs):
        from codex_session_relay.errors import RelayError

        with self.assertRaises(RelayError) as caught:
            callable_(*args, **kwargs)
        self.assertEqual(
            caught.exception.reason, reason,
            f"expected {reason} but got {caught.exception.reason}: {caught.exception.detail}",
        )
        return caught.exception


class DeliveryTestCase(RelayTestCase):
    """A registered relationship, a final event, and a fake host wired to all of it."""

    def setUp(self):
        super().setUp()
        from codex_session_relay.ack import AckService
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.fakehost import FakeHostAdapter
        from codex_session_relay.reconcile import Reconciler

        self.adapter = FakeHostAdapter(self.clock)
        self.adapter.add_thread(PARENT)
        self.adapter.add_thread(CHILD)
        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.ack = AckService(self.store, self.registry, self.intake, self.delivery, self.clock)
        self.reconciler = Reconciler(self.store, self.registry, self.delivery, self.clock)

    def ready_event(self, *, text="the deliverable", recipients=None, register=True,
                    settings=_UNSET):
        relationship = self.register(
            recipients=recipients, settings=settings,
        ) if register else self.registry.get(
            self.registry.register.__self__ and self._rid
        )
        self._rid = relationship["relationshipId"]
        path = self.artifact("out.txt", text)
        payload = self.ready_payload(relationship, [path])
        self.accept(payload)
        return relationship, payload["eventId"]

    def queued_event(self, **kwargs):
        relationship, event_id = self.ready_event(**kwargs)
        self.delivery.enqueue(event_id)
        return relationship, event_id

    def attempt(self, event_id, *, now=None):
        return self.delivery.attempt(event_id, self.adapter, now=now)

    def delivery_row(self, event_id):
        return self.delivery.get(event_id)

    def attempts_for(self, event_id):
        return self.store.all(
            "SELECT * FROM attempts WHERE event_id = ? ORDER BY attempt_no", (event_id,)
        )
