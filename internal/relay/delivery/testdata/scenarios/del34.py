from codex_session_relay.bridge_adapter import BridgeHostAdapter
from tests.test_bridge_adapter import FakeRpc, emulated_thread_list
from tests.test_delivery import ExecAwareHost
def host(archived=False):
    threads = [{"id": PARENT, "source": "vscode"}, {"id": CHILD, "source": "exec", "archived": archived}]
    return ExecAwareHost(c.adapter, BridgeHostAdapter(call=FakeRpc({"thread/list": emulated_thread_list(threads)}), store=c.store, clock=c.clock))
_completion, correction = c.correction_after_needs_changes()
out["correction"] = correction
mode = sys.argv[3]
if mode == "live":
    out["record"] = c.delivery.attempt(correction, host())
else:
    if mode == "legacy":
        c.store.db.execute("INSERT INTO failed_operations (scope_key, operation, detail, error_code, occurred_at) VALUES (?, 'lifecycle_read', 'lifecycle_unknown', 'lifecycle_unknown', ?)", (correction, c.clock.iso()))
        c.store.db.commit()
        c.clock.advance(1)
    out["record"] = c.delivery.attempt(correction, host(True))
