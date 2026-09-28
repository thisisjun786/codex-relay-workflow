import asyncio
import json
import tempfile
from pathlib import Path

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger

root = Path(tempfile.mkdtemp())
class RPC:
    async def call(self, method, params):
        if method == "thread/read": return {"thread": {"status": {"type": "idle"}}}
        if method == "thread/resume": return json.load(open(0))
        raise AssertionError(method)
    async def close(self): pass

# Read fixture before the worker starts using stdin.
settings = json.loads(input())
resume = json.loads(input())
class FixedRPC:
    async def call(self, method, params):
        if method == "thread/read": return {"thread": {"status": {"type": "idle"}}}
        if method == "thread/resume": return resume
        raise AssertionError(method)
    async def close(self): pass

adapter = BridgeHostAdapter(str(root/"socket"), timeout=.02, caller_slack=1,
    execution_policy=None, app_server_factory=lambda _: FixedRPC(),
    ledger_factory=lambda: (root/"socket", Ledger(root/"ops.sqlite3")))
async def guard(_rpc):
    await asyncio.sleep(1)
try:
    try:
        adapter.send_message("deadline", "thread", "message", TaskSettings(settings),
            before_start=guard, guard_rpc_requests=1)
    except Exception as error:
        caller = {"error": type(error).__name__, "detail": str(error)}
    receipt = adapter._transport.ledger_get("deadline")
    print(json.dumps({"caller": caller, "receipt": receipt}, sort_keys=True, default=str))
finally:
    adapter.close()
