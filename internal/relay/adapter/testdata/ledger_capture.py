import json
import os
import sys
from pathlib import Path
from codex_session_relay.bridge_adapter import BridgeHostAdapter
from codex_thread_bridge.execution import PRESENCE_ONLY

spec = json.load(sys.stdin)
class Idle:
    async def call(self, method, params):
        raise AssertionError(f"unexpected {method}")
    async def close(self):
        pass
options = dict(execution_policy=PRESENCE_ONLY, app_server_factory=lambda _: Idle())
if spec.get("pin"):
    options["ledger_directory"] = spec["directory"]
with BridgeHostAdapter(spec["socket"], **options) as adapter:
    first = adapter.ledger_identity_record()
    if spec.get("replace"):
        path = Path(first["realPath"])
        data = path.read_bytes()
        path.unlink()
        path.write_bytes(data)
    try:
        answer = adapter.require_ledger(first)
    except Exception as error:
        answer = {"error": str(error)}
    print(json.dumps(answer, sort_keys=True))
