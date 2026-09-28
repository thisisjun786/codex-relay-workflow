import json
import sys
import tempfile
from pathlib import Path

from codex_session_relay.clock import FakeClock
from codex_session_relay.lifecycle import Lifecycle, record
from codex_session_relay.store import Store

results = []
for raw in json.load(sys.stdin):
    value = json.loads(raw)
    store = Store(Path(tempfile.mkdtemp()) / "relay.sqlite3")
    try:
        try:
            record(store, FakeClock(1700000000), Lifecycle(raw, "idle", False, None, value, "yes", None, ""))
            row = store.one("SELECT can_accept_input, typeof(can_accept_input) AS kind FROM recipient_lifecycle")
            results.append({"value": row["can_accept_input"], "kind": row["kind"]})
        except Exception as error:
            results.append({"error": type(error).__name__, "detail": str(error)})
    finally:
        store.close()
json.dump(results, sys.stdout, allow_nan=True)
