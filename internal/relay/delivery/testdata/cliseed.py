"""Seed a relationship for the CLI parity test, through the real Python registry."""
import os, sys
from codex_session_relay.clock import FakeClock
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import Registry, record_settings
from codex_session_relay.store import Store
state, root = sys.argv[1], sys.argv[2]
os.makedirs(root, exist_ok=True)
store = Store(os.path.join(state, "relay.sqlite3"))
clock = FakeClock()
rel = Registry(store, clock).register(parent=Endpoint("01parent-task", "host-a", cwd="/parent"), child=Endpoint("01child-task", "host-a", cwd=root), issue_key="REL-1", artifact_roots=[root], allowed_recipients=["01parent-task", "01child-task"], dispatch_request_id="dispatch-1", dispatch_turn_id="turn-dispatch-1")
print(rel["relationshipId"])
