import os
from codex_session_relay.models import Endpoint
from tests.support import HOST
def other():
    root = os.path.join(c.tmp, "other-project"); os.makedirs(root, exist_ok=True)
    return c.registry.register(parent=Endpoint("01other-parent", HOST, cwd="/other", cxc_session="cxc-other"),
        child=Endpoint("01other-child", HOST, cwd=root, cxc_session="cxc-other-c"), issue_key="REL-2",
        artifact_roots=[root], allowed_recipients=["01other-parent"], dispatch_request_id="dispatch-2", dispatch_turn_id="turn-dispatch-2")
mode = sys.argv[3] if len(sys.argv) > 3 else ""
if mode == "scope":
    _r, e = c.ready_event()
    out["refused"] = refusal(c.delivery.enqueue, e, recipient_task_id="somebody-else")
elif mode == "other":
    o = other()
    _r, e = c.ready_event(recipients=[PARENT, o["parent"]["taskId"]])
    out["refused"] = refusal(c.delivery.enqueue, e, recipient_task_id=o["parent"]["taskId"])
    out["row"] = c.delivery.find(e)
elif mode == "tampered":
    o = other()
    _r, e = c.queued_event(recipients=[PARENT, o["parent"]["taskId"]])
    c.adapter.add_thread(o["parent"]["taskId"])
    with c.store.transaction() as db:
        db.execute("UPDATE deliveries SET recipient_task_id = ?, recipient_thread_id = ? WHERE event_id = ?", (o["parent"]["taskId"], o["parent"]["taskId"], e))
    out["refused"] = refusal(c.attempt, e)
else:
    other()
    _r, e = c.queued_event()
    out["record"] = c.attempt(e)
