"""Managed entry drives actual registry/criteria/marker state with a boundary fake host."""

import copy
import json
import os
from pathlib import Path
from unittest.mock import patch

from codex_session_relay import rolepolicy
from codex_session_relay.hostadapter import ThreadFacts, TurnInfo
from codex_session_relay.managed import ManagedStart, parse_request, operation_ids
from codex_session_relay.errors import RegistrationError

from .support import RelayTestCase, task_settings, PARENT, HOST
from .test_rolepolicy import write_policy, PARENT_MODEL, PARENT_EFFORT


class Host:
    def __init__(self, settings):
        self.settings = settings
        self.operations = {}
        self.created = self.sent = 0
        self.standby = "completed"
        self.paused = False
        self.creation_status = "accepted"
        self.ledger_path = Path(settings["cwd"]) / "test-operations-ledger"
        self.ledger_path.touch()

    def ledger_identity_record(self):
        from codex_session_relay.bridge_adapter import ledger_identity
        return ledger_identity(self.ledger_path)

    def require_ledger(self, expected):
        from codex_session_relay.hostadapter import HostUnavailable
        actual = self.ledger_identity_record()
        if tuple(expected[key] for key in ("realPath", "device", "inode")) != tuple(
                actual[key] for key in ("realPath", "device", "inode")):
            raise HostUnavailable("ledger replaced")
        return actual

    def get_operation(self, request):
        return self.operations.get(request)

    def create_thread(self, request, **kwargs):
        self.created += 1
        assert "business-secret" not in kwargs["prompt"]
        data = copy.deepcopy(self.settings)
        environments = data.pop("environments")
        receipt = {"status": self.creation_status, "threadId": "child-new", "turnId": "standby",
                   "creation": {**data, "thread": {"id": "child-new", "environments": environments}}}
        self.operations[request] = receipt
        return receipt

    def read_turn(self, task, turn):
        return TurnInfo(turn, self.standby)

    def read_thread(self, task):
        return ThreadFacts("idle", True)

    def is_archived(self, task, **kwargs):
        return False

    def read_goal_status(self, task):
        return "paused" if self.paused else None

    def send_message(self, request, task, message, settings, *, before_start=None,
                     guard_rpc_requests=0):
        assert guard_rpc_requests == 10
        self.sent += 1
        receipt = {"status": "accepted", "threadId": task, "turnId": "business", "requestId": request}
        self.operations[request] = receipt
        return receipt


class ManagedEntry(RelayTestCase):
    def setUp(self):
        super().setUp()
        path = write_policy(Path(self.tmp))
        self.env = patch.dict(os.environ, {rolepolicy.ENVIRONMENT_VARIABLE: path})
        self.env.start()
        self.addCleanup(self.env.stop)
        self.addCleanup(rolepolicy.reset)
        child = task_settings(self.root, environments=[])
        self.request = {
            "schema": "managed-start/1", "requestId": "managed-1", "issueKey": "REL-MANAGED",
            "parent": {"taskId": PARENT, "hostId": HOST,
                       "settings": task_settings(self.root, model=PARENT_MODEL,
                                                  reasoningEffort=PARENT_EFFORT, environments=[])},
            "child": {"hostId": HOST, "title": "REL-MANAGED · Verify admission", "settings": child},
            "artifactRoots": [self.root], "allowedRecipients": [PARENT],
            "criteria": [{"id": "c1", "title": "preserve replay identity", "required": True}],
            "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline",
            "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret: implement the scoped fix",
        }
        self.host = Host(child)
        self.observation = {"observed": True, "policy": rolepolicy.declared().summary()}
        self.start = ManagedStart(self.store, self.clock, self.host, lambda: self.observation,
                                  socket=os.path.join(self.tmp, "socket"),
                                  marker_root=os.path.join(self.tmp, "markers"))

    def test_same_request_recovers_one_child_one_business_turn(self):
        first = self.start.run(self.request)
        self.assertEqual(first["state"], "admitted")
        self.assertEqual(set(first["selectors"]), {"state", "markerRoot", "workspace"})
        self.assertTrue(Path(first["selectors"]["state"]).is_absolute())
        argv = first["reportingArgv"]
        from codex_session_relay.cli import build_parser
        namespace = build_parser().parse_args(argv)
        self.assertEqual(namespace.command, "reporting-show")
        self.assertEqual(namespace.state, first["selectors"]["state"])
        self.assertEqual(namespace.turn, first["businessTurnId"])
        self.assertEqual(namespace.session, first["childTaskId"])
        self.assertEqual(namespace.assignment, first["assignmentId"])
        second = self.start.run(self.request)
        self.assertEqual(second["businessTurnId"], first["businessTurnId"])
        self.assertEqual((self.host.created, self.host.sent), (1, 1))
        self.assertEqual(second["childClaim"], "not_observed")

    def test_a_result_without_a_business_turn_carries_no_reporting_argv(self):
        self.observation = {"observed": False, "reason": "worker_policy_unconfigured"}
        refused = self.start.run(self.request)
        self.assertNotIn("businessTurnId", refused)
        self.assertNotIn("reportingArgv", refused)
        self.assertEqual(set(refused["selectors"]), {"state", "markerRoot", "workspace"})

    def test_crash_after_business_send_replays_its_receipt_without_another_turn(self):
        with patch("codex_session_relay.admission.admit_explicitly", side_effect=RuntimeError("caller died")):
            with self.assertRaisesRegex(RuntimeError, "caller died"):
                self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 1))
        recovered = self.start.run(self.request)
        self.assertEqual(recovered["businessTurnId"], "business")
        self.assertEqual(recovered["state"], "admitted")
        self.assertEqual((self.host.created, self.host.sent), (1, 1))

    def test_crash_after_registration_resumes_same_shell_and_finishes_criteria(self):
        with patch.object(self.start.criteria, "ensure_registered", side_effect=RuntimeError("caller died")):
            with self.assertRaisesRegex(RuntimeError, "caller died"):
                self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 0))
        recovered = self.start.run(self.request)
        self.assertEqual(recovered["state"], "admitted")
        self.assertEqual((self.host.created, self.host.sent), (1, 1))

    def partial_creation(self, *, attempted_turn=False):
        real_create = self.host.create_thread
        def partial(request, **kwargs):
            receipt = real_create(request, **kwargs)
            receipt.update(status="failed", attemptedEffects=["thread/start", "thread/name/set"])
            receipt.pop("turnId")
            if attempted_turn:
                receipt["attemptedEffects"].append("turn/start")
            return receipt
        self.host.create_thread = partial

    def test_failed_naming_recovers_standby_on_same_shell(self):
        self.partial_creation()
        original_send = self.host.send_message
        def send(request, task, message, settings, **kwargs):
            receipt = original_send(request, task, message, settings, **kwargs)
            if request.startswith("managed-standby-"):
                receipt["turnId"] = "recovered-standby"
            return receipt
        self.host.send_message = send
        result = self.start.run(self.request)
        self.assertEqual(result["state"], "admitted", result)
        self.assertEqual(result["standbyTurnId"], "recovered-standby")
        self.assertEqual((self.host.created, self.host.sent), (1, 2))
        replay = self.start.run(self.request)
        self.assertEqual(replay["state"], "admitted", replay)
        self.assertEqual((self.host.created, self.host.sent), (1, 2))
        original = self.host.operations[result["creationRequestId"]]
        self.assertEqual(original["status"], "failed")
        self.assertNotIn("turnId", original)

    def test_uncertain_first_turn_is_not_replaced_with_recovery_turn(self):
        self.partial_creation(attempted_turn=True)
        first = self.start.run(self.request)
        self.assertEqual(first["state"], "incomplete")
        self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_paused_partial_shell_is_preserved_without_recovery_send(self):
        self.partial_creation()
        self.host.paused = True
        result = self.start.run(self.request)
        self.assertEqual(result["state"], "incomplete")
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_recovery_send_unknown_is_retained_not_resent(self):
        self.partial_creation()
        def unknown(request, task, *args, **kw):
            self.host.sent += 1
            receipt = {"status": "outcome_unknown", "threadId": task}
            self.host.operations[request] = receipt
            return receipt
        self.host.send_message = unknown
        self.start.run(self.request)
        self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 1))

    def test_worker_absent_never_creates_or_reserves(self):
        self.observation = {"observed": False, "reason": "worker_policy_unconfigured"}
        result = self.start.run(self.request)
        self.assertEqual(result["state"], "refused")
        self.assertEqual(self.host.created, 0)
        self.assertIsNone(self.registry.start_request("managed-1"))

    def test_unsupported_creation_policy_never_arms_or_creates(self):
        self.request["child"]["settings"]["sandbox"] = {"type": "readOnly", "networkAccess": True}
        with self.assertRaises(ValueError):
            self.start.run(self.request)
        self.assertIsNone(self.registry.start_request("managed-1"))
        self.assertEqual(self.host.operations, {})

    def test_standby_incomplete_retains_registered_child_until_retry(self):
        self.host.standby = "inProgress"
        result = self.start.run(self.request)
        self.assertEqual(result["reason"], "standby_incomplete")
        self.assertEqual(self.host.sent, 0)
        self.host.standby = "completed"
        self.assertEqual(self.start.run(self.request)["state"], "admitted")
        self.assertEqual(self.host.created, 1)

    def test_unknown_creation_is_retained_without_a_second_create(self):
        self.host.creation_status = "outcome_unknown"
        self.assertEqual(self.start.run(self.request)["reason"], "creation_unknown")
        self.assertEqual(self.start.run(self.request)["reason"], "creation_unknown")
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_registered_child_can_receive_revisions_but_an_unrelated_task_cannot(self):
        from codex_session_relay.scope import assert_assignment_delivery, ScopeError
        result = self.start.run(self.request)
        relation = self.registry.get(result["relationshipId"])
        self.assertEqual(relation["authorizedScope"]["allowedRecipients"], [PARENT, "child-new"])
        assert_assignment_delivery(relation, kind="revision_request", recipient_task_id="child-new")
        assert_assignment_delivery(relation, kind="completion_event", recipient_task_id=PARENT)
        with self.assertRaises(ScopeError):
            assert_assignment_delivery(relation, kind="revision_request", recipient_task_id="unrelated")
        self.assertEqual(self.request["allowedRecipients"], [PARENT])
        self.assertEqual(self.start.run(self.request)["state"], "admitted")

    def test_child_recipient_is_not_added_twice_when_already_declared(self):
        self.request["allowedRecipients"].append("child-new")
        result = self.start.run(self.request)
        relation = self.registry.get(result["relationshipId"])
        self.assertEqual(relation["authorizedScope"]["allowedRecipients"], [PARENT, "child-new"])

    def test_retry_with_a_replaced_ledger_cannot_create_another_child(self):
        self.host.creation_status = "outcome_unknown"
        self.start.run(self.request)
        replacement = self.host.ledger_path.with_suffix(".replacement")
        replacement.touch()
        replacement.replace(self.host.ledger_path)
        self.host.operations.clear()
        with self.assertRaises(RegistrationError):
            self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_real_bridge_recovers_naming_failure_without_another_shell(self):
        from codex_session_relay.bridge_adapter import BridgeHostAdapter
        from codex_thread_bridge.effects import mark_sent
        from codex_thread_bridge.rpc import RpcError

        settings = copy.deepcopy(self.request["child"]["settings"])
        turns, calls = [], []

        class Rpc:
            async def call(inner, method, params):
                calls.append(method)
                mark_sent(method)
                if method in ("thread/start", "thread/resume"):
                    response = copy.deepcopy(settings)
                    environments = response.pop("environments")
                    return {**response, "thread": {"id": "retained-shell", "environments": environments}}
                if method == "thread/name/set":
                    raise RpcError(method, {"code": "naming_failed", "message": "injected naming failure"})
                if method == "thread/read":
                    return {"thread": {"id": "retained-shell", "status": {"type": "idle"},
                                       "canAcceptDirectInput": True}}
                if method == "thread/list":
                    return {"data": [] if params.get("archived") else [{"id": "retained-shell"}]}
                if method == "thread/goal/get":
                    return {"goal": None}
                if method == "turn/start":
                    turn = {"id": "real-turn-" + str(len(turns)), "status": "completed"}
                    turns.append(turn)
                    return {"turn": turn}
                if method == "thread/turns/list":
                    return {"data": turns, "nextCursor": None}
                raise AssertionError(method)

            async def close(inner):
                pass

        adapter = BridgeHostAdapter(self.start.socket, ledger_directory=Path(self.tmp) / "bridge-ledger",
                                    app_server_factory=lambda _: Rpc(), timeout=3)
        self.addCleanup(adapter.close)
        start = ManagedStart(self.store, self.clock, adapter, lambda: self.observation,
                             socket=self.start.socket, marker_root=self.start.marker_root)
        result = start.run(self.request)
        self.assertEqual(result["state"], "admitted", result)
        self.assertEqual(result["childTaskId"], "retained-shell")
        self.assertEqual(calls.count("thread/start"), 1)
        self.assertEqual(len(turns), 2)  # standby recovery, then registered business
        creation = adapter.get_operation(operation_ids(self.request["requestId"])[0])
        self.assertEqual(creation["status"], "failed")
        self.assertNotIn("turn/start", creation["attemptedEffects"])
        replay = start.run(self.request)
        self.assertEqual(replay["businessTurnId"], result["businessTurnId"])
        self.assertEqual((calls.count("thread/start"), len(turns)), (1, 2))

    def test_ledger_replacement_after_preflight_refuses_before_creation(self):
        from codex_session_relay.hostadapter import HostUnavailable
        original = self.start.registry.arm_start

        def replace_after_arm(*args):
            row = original(*args)
            replacement = self.host.ledger_path.with_suffix(".replacement")
            replacement.touch()
            replacement.replace(self.host.ledger_path)
            return row

        with patch.object(self.start.registry, "arm_start", side_effect=replace_after_arm):
            with self.assertRaises(HostUnavailable):
                self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (0, 0))

    def test_an_unknown_ledger_refuses_before_reserving_or_creating(self):
        with patch.object(self.host, "ledger_identity_record", return_value=None):
            with self.assertRaises(ValueError):
                self.start.run(self.request)
        self.assertIsNone(self.registry.start_request(self.request["requestId"]))
        self.assertEqual((self.host.created, self.host.sent), (0, 0))

    def test_changed_business_prompt_refuses_before_dispatch(self):
        self.host.standby = "inProgress"
        self.start.run(self.request)
        self.request["prompt"] += " widened"
        with self.assertRaises(RegistrationError):
            self.start.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_original_selector_spellings_are_part_of_replay_identity(self):
        self.host.standby = "inProgress"
        self.start.run(self.request)
        alias = Path(self.tmp) / "spelling"
        alias.mkdir()
        variants = (
            {"socket": str(alias) + "/../socket"},
            {"marker_root": str(alias) + "/../markers"},
            {"state_selector": str(alias) + "/.."},
        )
        for changed in variants:
            with self.subTest(changed=changed):
                kwargs = {"socket": self.start.socket, "marker_root": self.start.marker_root,
                          "state_selector": str(self.store.path.parent), **changed}
                replay = ManagedStart(self.store, self.clock, self.host,
                                      lambda: self.observation, **kwargs)
                with self.assertRaises(RegistrationError):
                    replay.run(self.request)
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_paused_child_is_not_sent_business(self):
        self.host.paused = True
        result = self.start.run(self.request)
        self.assertEqual(result["reason"], "recipient_paused")
        self.assertEqual(self.host.sent, 0)

    def test_registration_drift_never_reactivates_on_replay(self):
        self.host.standby = "inProgress"
        first = self.start.run(self.request)
        self.registry.set_status(first["relationshipId"], "paused", actor=PARENT)
        result = self.start.run(self.request)
        self.assertEqual(result["reason"], "relationship_not_active")
        self.assertEqual(self.host.sent, 0)

    def test_input_snapshot_and_stable_bounded_operation_ids(self):
        parsed = parse_request(self.request)
        self.request["criteria"][0]["title"] = "changed later"
        self.assertEqual(parsed["criteria"][0]["title"], "preserve replay identity")
        create, business = operation_ids("x" * 128)
        self.assertNotEqual(create, business)
        self.assertLessEqual(len(create), 128)
        self.assertLessEqual(len(business), 128)

    def test_policy_disappearing_after_creation_preserves_shell_and_withholds_business(self):
        original = self.host.create_thread
        def create(*args, **kwargs):
            receipt = original(*args, **kwargs)
            self.observation = {"observed": False, "reason": "worker_policy_unconfigured"}
            return receipt
        self.host.create_thread = create
        result = self.start.run(self.request)
        self.assertEqual(result["reason"], "worker_policy_unconfigured")
        self.assertEqual((self.host.created, self.host.sent), (1, 0))
        self.observation = {"observed": True, "policy": rolepolicy.declared().summary()}
        self.assertEqual(self.start.run(self.request)["state"], "admitted")
        self.assertEqual((self.host.created, self.host.sent), (1, 1))

    def test_readback_settings_drift_is_not_overwritten(self):
        self.host.standby = "inProgress"
        result = self.start.run(self.request)
        from codex_session_relay.registry import record_settings, load_settings
        changed = copy.deepcopy(self.request["child"]["settings"])
        changed["sandbox"]["networkAccess"] = True
        record_settings(self.store, self.clock, result["childTaskId"], changed,
                        source="user_transition", role="child")
        with self.assertRaises(RegistrationError):
            self.start.run(self.request)
        self.assertTrue(load_settings(self.store, result["childTaskId"]).data["sandbox"]["networkAccess"])
        self.assertEqual(self.host.sent, 0)

    def test_changed_creation_environment_refuses_before_registration_and_business(self):
        self.host.settings = copy.deepcopy(self.request["child"]["settings"])
        self.host.settings["environments"] = [
            {"environmentId": "local", "cwd": self.root, "runtimeWorkspaceRoots": [self.root]}]
        result = self.start.run(self.request)
        self.assertEqual(result["reason"], "creation_settings_unverified")
        self.assertEqual(self.host.sent, 0)
        self.assertEqual(self.store.one("SELECT count(*) FROM relationships")[0], 0)

    def test_every_behavior_input_change_rejects_same_request_without_business(self):
        self.host.standby = "inProgress"
        self.start.run(self.request)
        variations = []
        for key, value in (("criteriaSource", "new-source"), ("scopeRef", "new-scope"),
                           ("baselineRevision", "new-base"), ("prompt", "different")):
            changed = copy.deepcopy(self.request)
            changed[key] = value
            variations.append(changed)
        changed = copy.deepcopy(self.request)
        changed["criteria"][0]["required"] = False
        variations.append(changed)
        for changed in variations:
            with self.subTest(request=changed["requestId"], prompt=changed["prompt"]):
                with self.assertRaises(RegistrationError):
                    self.start.run(changed)
        self.assertEqual((self.host.created, self.host.sent), (1, 0))

    def test_real_guard_observes_a_pause_after_resume_without_using_main_connection(self):
        import asyncio
        import threading
        self.host.standby = "inProgress"
        result = self.start.run(self.request)
        task = result["childTaskId"]
        class Rpc:
            async def call(self, method, params):
                if method == "thread/list":
                    return {"data": [] if params["archived"] else [{"id": task}], "nextCursor": None}
                if method == "thread/goal/get":
                    return {"goal": {"status": "paused"}}
                raise AssertionError(method)
        found = {}
        def run():
            try:
                found["result"] = asyncio.run(self.start._before_start(Rpc()))
            except BaseException as error:
                found["error"] = error
        worker = threading.Thread(target=run)
        worker.start()
        worker.join(5)
        self.assertFalse(worker.is_alive())
        self.assertNotIn("error", found)
        self.assertEqual(found["result"]["code"], "recipient_paused")

    def test_host_guard_budget_covers_both_full_listings_and_final_reads(self):
        import asyncio
        from codex_session_relay.managed import HOST_READY_RPC_REQUESTS
        calls, pages = [], {True: 0, False: 0}

        class Rpc:
            async def call(self, method, params):
                calls.append(method)
                if method == "thread/list":
                    archived = params["archived"]
                    pages[archived] += 1
                    page = pages[archived]
                    return {"data": [{"id": "child-new"}] if not archived and page == 4 else [],
                            "nextCursor": str(page) if page < 4 else None}
                if method == "thread/goal/get":
                    return {"goal": None}
                if method == "thread/read":
                    return {"thread": {"status": {"type": "idle"}, "canAcceptDirectInput": True}}
                raise AssertionError(method)

        self.assertIsNone(asyncio.run(self.start._host_ready(Rpc(), "child-new")))
        self.assertEqual(pages, {True: 4, False: 4})
        self.assertEqual(len(calls), 10)
        self.assertEqual(HOST_READY_RPC_REQUESTS, len(calls))

    def test_cli_missing_worker_is_refused_without_creating_the_missing_store(self):
        import subprocess
        import sys
        destination = Path(self.tmp) / "absent-state"
        request = Path(self.tmp) / "request.json"
        request.write_text(json.dumps(self.request))
        result = subprocess.run([
            sys.executable, "-m", "codex_session_relay.cli", "--state", str(destination),
            "--socket", str(Path(self.tmp) / "socket"), "managed-start", "--request", "@" + str(request),
            "--marker-root", str(Path(self.tmp) / "markers"),
        ], capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout)["state"], "refused")
        self.assertFalse((destination / "relay.sqlite3").exists())

    def test_cli_rejects_unknown_input_without_any_rpc(self):
        import subprocess
        import sys
        invalid = {**self.request, "overridePermissions": True}
        result = subprocess.run([
            sys.executable, "-m", "codex_session_relay.cli", "--state", str(Path(self.tmp) / "absent"),
            "--socket", str(Path(self.tmp) / "socket"), "managed-start", "--request", json.dumps(invalid),
            "--marker-root", str(Path(self.tmp) / "markers"),
        ], capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 4, result.stdout + result.stderr)
        self.assertIn("unknown", json.loads(result.stdout)["detail"])

    def test_show_does_not_create_an_absent_store_and_retains_dispatch_identity(self):
        from argparse import Namespace
        from types import SimpleNamespace
        from codex_session_relay.cli import cmd_managed_show
        from codex_session_relay.store import resolve_state_dir
        absent = Path(self.tmp) / "absent-show"
        services = SimpleNamespace(selection=resolve_state_dir(str(absent)))
        report = cmd_managed_show(services, Namespace(request_id="managed-1"))
        self.assertFalse(report["readable"])
        self.assertIsNone(report["request"])
        self.assertFalse(absent.exists())

        self.start.run(self.request)
        services.selection = resolve_state_dir(str(self.store.path.parent))
        report = cmd_managed_show(services, Namespace(request_id="managed-1"))
        self.assertTrue(report["readable"])
        self.assertEqual(report["request"]["child_task_id"], "child-new")
        self.assertEqual(report["lastObservation"]["businessTurnId"], "business")

    def test_final_guard_rejects_owner_or_scope_changed_during_resume(self):
        import asyncio
        self.host.standby = "inProgress"
        result = self.start.run(self.request)
        task = result["childTaskId"]
        class Rpc:
            async def call(self, method, params):
                if method == "thread/list":
                    return {"data": [] if params["archived"] else [{"id": task}], "nextCursor": None}
                if method == "thread/goal/get":
                    return {"goal": None}
                if method == "thread/read":
                    return {"thread": {"id": task, "status": {"type": "idle"}, "canAcceptDirectInput": True}}
                raise AssertionError(method)
        changes = {"parent_task_id": "replacement-parent", "child_host_id": "other-host",
                   "artifact_roots": json.dumps(["/other-scope"]),
                   "allowed_recipients": json.dumps([PARENT])}
        for column, value in changes.items():
            with self.subTest(column=column):
                previous = self.store.one("SELECT * FROM relationships WHERE relationship_id=?",
                                          (result["relationshipId"],))[column]
                with self.store.transaction() as db:
                    db.execute(f"UPDATE relationships SET {column}=? WHERE relationship_id=?",
                               (value, result["relationshipId"]))
                verdict = asyncio.run(self.start._before_start(Rpc()))
                with self.store.transaction() as db:
                    db.execute(f"UPDATE relationships SET {column}=? WHERE relationship_id=?",
                               (previous, result["relationshipId"]))
                self.assertIsNotNone(verdict, "resume-time authorization change escaped the final guard")
                self.assertEqual(verdict["code"], "managed_scope_changed")
