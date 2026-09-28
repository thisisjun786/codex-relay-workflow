import asyncio
import json
import tempfile
from pathlib import Path

from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_thread_bridge.ledger import Ledger

settings = json.loads(input())
root = Path(tempfile.mkdtemp())
class RPC:
    async def call(self, method, params):
        raise ConnectionRefusedError(111, "Connection refused")
    async def close(self): pass
adapter = BridgeHostAdapter(str(root/"socket"), execution_policy=None,
    app_server_factory=lambda _: RPC(), ledger_factory=lambda: (root/"socket", Ledger(root/"ops.sqlite3")))
try:
    receipt = adapter.send_message("refused", "thread", "message", TaskSettings(settings))
    print(json.dumps(receipt, sort_keys=True))
finally:
    adapter.close()
