"""Live Python oracle for todo 28; all state is supplied by the Go caller."""
import dataclasses
import json
import sys
from pathlib import Path
from unittest.mock import patch

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.clock import FakeClock
from codex_session_relay.settings import TaskSettings
from codex_session_relay.store import Store
from codex_thread_bridge.ledger import Ledger

spec = json.load(sys.stdin)
calls = []
answers = iter(spec.get("answers") or [])


def call(method, params):
    calls.append([method, params])
    answer = next(answers)
    if "rawResponse" in answer:
        return answer["rawResponse"]
    if "rpcError" in answer:
        from codex_thread_bridge.rpc import RpcError
        raise RpcError(method, answer["rpcError"])
    if answer.get("creation"):
        cwd = params["cwd"]
        return dict(sorted({"thread":{"id":"thread-created-1"}, "cwd":cwd, "approvalPolicy":"never", "model":"gpt-5.4", "reasoningEffort":"medium", "runtimeWorkspaceRoots":[cwd], "sandbox":{"networkAccess":False,"type":"readOnly"}}.items()))
    if "error" in answer:
        raise RuntimeError(answer["error"])
    if "phase" in answer:
        from codex_thread_bridge.rpc import PhaseTimeout
        phase = answer["phase"]
        detail = {"establish": f"connection establishment exceeded 0.25s; no {method} frame was sent", "transmit": "the request frame did not drain within 0.25s and the connection was retired; response unavailable; do not resend", "ack": "response unavailable; do not resend"}[phase]
        raise PhaseTimeout(method, phase, 0.25, detail)
    return answer


class RPC:
    async def call(self, method, params):
        return call(method, params)

    async def close(self):
        pass


root = Path(spec["root"])
store = Store(root / "python-store.sqlite3") if spec.get("store") else None
options = {"store": store, "clock": FakeClock(), "page": spec.get("page", 50)}
if "settings" in spec:
    options.update(
        app_server_factory=lambda _: RPC(),
        ledger_factory=lambda: (root / "socket", Ledger(root / "python-operations.sqlite3")),
        execution_policy=None,
    )
else:
    options["call"] = call
if spec.get("policy"):
    from codex_thread_bridge.execution import ExecutionPolicy
    options["execution_policy"] = ExecutionPolicy.from_bytes(spec["policy"].encode(), source="policy")
adapter = BridgeHostAdapter(str(root / "socket"), **options)
results = []
receipt_bytes = []
attempt_bytes = []
# Both runtimes receive the same fixed clock before the operation starts.
try:
    with patch("codex_thread_bridge.ledger.time.time", return_value=spec["now"]):
        for action in spec["actions"]:
            method = action[0]
            try:
                if method == "begin":
                    side = Ledger(root / "python-operations.sqlite3")
                    try: side.begin(action[1], "send_message_to_thread", {"threadId":action[2], "message":action[3]})
                    finally: side.close()
                    result = None
                elif method == "no-settings":
                    result = adapter.send_message(*action[1:], None)
                elif method == "create":
                    result = adapter.create_thread(action[1], cwd=str(root), title=action[2], model=action[3], reasoning_effort="medium", sandbox="read-only", role=action[4] or None)
                elif method in ("send", "guard"):
                    settings = TaskSettings(spec["settings"])
                    settings.settings_free_resume = spec.get("settingsFree", False)
                    async def guard(rpc):
                        await rpc.call('thread/goal/get', {'threadId': action[2]})
                        return action[4]
                    result = adapter.send_message(*action[1:4], settings, before_start=guard if method == "guard" else None)
                elif method == "find":
                    result = adapter.find_token(action[1], action[2], limit=action[3])
                elif method == "archive":
                    result = adapter.is_archived(action[1], cwd=action[2])
                elif method == "turn":
                    result = adapter.read_turn(*action[1:])
                elif method == "turn-ids":
                    result = adapter.list_turn_ids(action[1])
                elif method == "thread":
                    result = adapter.read_thread(action[1])
                elif method == "lifecycle-observe":
                    from codex_session_relay.lifecycle import observe, record
                    assert store is not None, 'lifecycle-observe requires a store fixture'
                    observation = observe(adapter, action[1], require_evidence=True)
                    record(store, FakeClock(), observation)
                    result = {'observation': dataclasses.asdict(observation), 'row': dict(store.one('SELECT task_id,runtime_status,archived,goal_status,can_accept_input,deliverable,withhold_reason,detail,observed_at FROM recipient_lifecycle WHERE task_id=?', (action[1],)))}
                elif method == "lifecycle-record":
                    from codex_session_relay.lifecycle import Lifecycle, record
                    assert store is not None, 'lifecycle-record requires a store fixture'
                    facts = adapter.read_thread(action[1])
                    observation = Lifecycle(action[1], facts.runtime_status, None, None, facts.can_accept_input, 'unknown', None)
                    try:
                        record(store, FakeClock(), observation)
                        result = dict(store.one('SELECT runtime_status,can_accept_input FROM recipient_lifecycle WHERE task_id=?', (action[1],)))
                    except Exception as error:
                        result = {'error': f'{type(error).__name__}: {error}'}
                elif method == "goal":
                    result = adapter.read_goal_status(action[1])
                elif method == "operation":
                    result = adapter.get_operation(action[1])
                elif method == "fingerprint":
                    result = adapter.recipient_fingerprint(action[1])
                else:
                    raise AssertionError(method)
                if method in ("send", "guard", "operation", "create") and result is not None:
                    receipt_bytes.append(json.dumps(result))
                if method == "send" and result is not None:
                    from codex_session_relay.transport import classify_operation_receipt, attempt_record, assert_attempt_invariants
                    record = attempt_record(classify_operation_receipt(result), request_id=action[1], event_id="ev-1", attempt_no=1, recipient="01parent", status_before="unknown", observed_at="2026-09-22T00:00:00Z")
                    assert_attempt_invariants(record)
                    attempt_bytes.append(json.dumps(record))
                results.append(dataclasses.asdict(result) if dataclasses.is_dataclass(result) else result)
            except Exception as error:
                results.append({"error": str(error)})
    cursors = [] if store is None else [dict(row) for row in store.all("SELECT * FROM discovery_cursors ORDER BY task_id,listing")]
    print(json.dumps({"results": results, "calls": calls, "cursors": cursors, "receiptBytes": receipt_bytes, "attemptBytes": attempt_bytes}, sort_keys=True, indent=2))
finally:
    adapter.close()
    if store is not None:
        store.close()
