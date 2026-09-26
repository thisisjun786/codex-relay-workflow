import dataclasses
from codex_session_relay import identity
from codex_session_relay.settings import TaskSettings
from codex_session_relay.delivery import DeliveryService
from tests.support import task_settings
def on_request(path="/parent"):
    return task_settings(path, approvalPolicy="on-request")
m = sys.argv[3]
if m == "record":
    out["onRequest"] = refusal(TaskSettings(on_request()).require_usable)
    out["refused"] = [refusal(TaskSettings(task_settings("/parent", approvalPolicy=r)).require_usable) for r in ("untrusted", {"granular": {}}, None)]
    out["resume"] = [TaskSettings(r).resume_params("t-1") for r in (task_settings("/parent"), on_request())]
elif m == "woken":
    _r, e = c.queued_event(settings=on_request())
    c.adapter.threads[PARENT].approval_policy = "on-request"
    out["record"] = c.attempt(e)
    c.clock.advance(100000)
    out["eligible"] = c.delivery.eligible(now=c.clock.now())
elif m == "busy":
    _r, e = c.queued_event(settings=on_request())
    c.adapter.threads[PARENT].approval_policy = "on-request"
    c.adapter.script("busy")
    out["busy"] = c.attempt(e)
    row = c.delivery_row(e)
    out["record"] = c.attempt(e, now=row["next_eligible_at"])
elif m == "folded":
    _r, e = c.queued_event(settings=on_request())
    c.adapter.threads[PARENT].approval_policy = "on-request"
    existing = c.adapter.start_turn(PARENT, status="inProgress")
    c.adapter.script("steer_existing")
    out["record"] = c.attempt(e)
    proof = identity.ack_proof(e, existing.turn_id)
    out["acks"] = [c.ack.acknowledge(e, ack_turn_id=existing.turn_id, ack_proof=proof, accepted=True, adapter=c.adapter) for _ in range(2)]
    turns = c.adapter.threads[PARENT].turns
    turns[turns.index(existing)] = dataclasses.replace(existing, status="completed")
    c.adapter.restart()
    for thread in c.adapter.threads.values():
        thread.status = "notLoaded"
    restarted = DeliveryService(c.store, c.registry, c.intake, c.clock)
    c.clock.advance(100000)
    out["after"] = restarted.attempt(e, c.adapter, now=c.clock.now())
