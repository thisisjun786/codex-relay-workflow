import json
import sys

from codex_session_relay.bridge_adapter import BridgeHostAdapter

pages = json.load(sys.stdin)
for page in pages:
    adapter = object.__new__(BridgeHostAdapter)
    adapter._call = lambda method, params, page=page: {"data": page}
    print(adapter.recipient_fingerprint("thread"))
