import asyncio
import json
import sys
from pathlib import Path

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger

spec = json.load(sys.stdin)
root = Path(spec["root"])
root.mkdir()
ledger_path = root / "operations.sqlite3"
calls = []


class RPC:
    async def call(self, method, params):
        calls.append(method)
        if method == "thread/read":
            return {"thread": {"status": {"type": "idle"}}}
        if method == "thread/resume":
            return spec["resume"]
        if method == "turn/start":
            return {"turn": {"id": "turn-after-replacement"}}
        raise AssertionError(method)

    async def close(self):
        pass


adapter = BridgeHostAdapter(
    str(root / "socket"),
    execution_policy=None,
    app_server_factory=lambda _socket: RPC(),
    ledger_factory=lambda: (root / "socket", Ledger(ledger_path)),
)
identity = adapter.ledger_identity_record()


async def guard(_rpc):
    # This is the managed callback's first operation. Replacement after this check and before
    # return occupies the exact window between Python's last ledger validation and turn/start.
    adapter.require_ledger(identity)
    ledger_path.rename(ledger_path.with_suffix(".open"))
    ledger_path.write_bytes(b"replacement")
    return None


try:
    try:
        receipt = adapter.send_message(
            "replacement-window",
            "thread",
            "message",
            TaskSettings(spec["settings"]),
            before_start=guard,
            guard_rpc_requests=1,
        )
        result = {"returned": True, "status": receipt.get("status"), "turnId": receipt.get("turnId"), "calls": calls}
    except Exception:
        result = {"returned": False, "status": None, "turnId": None, "calls": calls}
    print(json.dumps(result, separators=(",", ":")))
finally:
    adapter.close()
