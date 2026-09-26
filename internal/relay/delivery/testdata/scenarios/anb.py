from unittest import mock
from codex_session_relay import identity
from codex_session_relay.delivery import REVISION
from codex_session_relay.models import TurnRef
def revision_pending():
    _r, e = c.queued_event(recipients=[PARENT, CHILD])
    c.attempt(e); c.clock.advance(5)
    t = c.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
    c.ack.acknowledge(e, ack_turn_id=t.turn_id, ack_proof=identity.ack_proof(e, t.turn_id), accepted=True, adapter=c.adapter)
    c.ack.record_verdict(e, verdict="needs_changes", verdict_turn_id="verdict-1")
    return c.store.one("SELECT * FROM deliveries WHERE kind = ?", (REVISION,))["event_id"]
def gen2():
    return c.registry.generation(c._rid, 2)
def child_receipt():
    rel = c.registry.get(c._rid)
    anchor = gen2()["dispatchTurnId"]
    turn = c.assigned_turn(thread=CHILD, turn=anchor or "turn-unbound")
    return refusal(c.accept, c.ready_payload(rel, [c.artifact("fixed.txt", "the correction")], generation=2, turn=turn))
def lost_settle_write():
    rev = revision_pending(); c.clock.advance(3600)
    c.adapter.script("in_progress")
    rec = c.delivery.attempt(rev, c.adapter, now=c.clock.now())
    turn = c.adapter.start_turn(CHILD, status="inProgress")
    c.adapter.ledger[rec["requestId"]] = {"requestId": rec["requestId"], "status": "accepted", "resumed": {"approvalPolicy": "never"}, "turnId": turn.turn_id}
    return rev, turn.turn_id
m = sys.argv[3]
if m in ("tick", "recovery", "unbound", "idempotent", "never", "retired_ready", "retired_failed", "retired_daemon"):
    rev = revision_pending(); c.clock.advance(3600)
    if m == "never": c.adapter.script("busy")
    out["record"] = c.delivery.attempt(rev, c.adapter, now=c.clock.now())
    out["pending"] = gen2()["anchorState"]
    if m == "unbound":
        out["receipt"] = child_receipt()
    elif m == "tick":
        # The tick's two binding passes around its reconciliation (daemon.tick, todo 29).
        out["bound"] = [c.ack.bind_pending_anchors(), c.reconciler.recover_on_start(c.adapter)["reconciled"], c.ack.bind_pending_anchors()]
        out["gen2"] = gen2(); out["receipt"] = child_receipt()
    elif m in ("recovery", "idempotent", "never"):
        out["bound"] = c.ack.bind_pending_anchors()
        out["again"] = c.ack.bind_pending_anchors()
        out["gen2"] = gen2()
        if m == "recovery": out["receipt"] = child_receipt()
    else:
        c.ack.bind_pending_anchors()
        if m == "retired_ready":
            out["receipt"] = child_receipt()
        elif m == "retired_failed":
            rel = c.registry.get(c._rid)
            turn = c.assigned_turn(thread=CHILD, turn=gen2()["dispatchTurnId"])
            out["receipt"] = refusal(c.accept, c.execution_payload(rel, "failed", generation=2, turn=turn))
        else:
            anchor = gen2()["dispatchTurnId"]
            c.adapter.start_turn(CHILD, turn_id=anchor, status="failed")
            out["receipt"] = c.intake.daemon_observation(c._rid, TurnRef(CHILD, anchor, "failed"))
        with c.store.transaction() as db:
            out["reason"] = c.delivery._supersession_reason(db, rev)
elif m == "reconcile":
    rev = revision_pending(); c.clock.advance(3600)
    c.adapter.script("transport_unknown")
    out["record"] = c.delivery.attempt(rev, c.adapter, now=c.clock.now())
    out["promoted"] = c.reconciler.recover_on_start(c.adapter)
    out["bound"] = c.ack.bind_pending_anchors()
    out["gen2"] = gen2(); out["state"] = c.delivery.get(rev)["state"]
elif m == "promotion":
    rev, turn = lost_settle_write()
    out["recovered"] = c.reconciler.recover_on_start(c.adapter)
    out["gen2"] = gen2(); out["receipt"] = child_receipt()
elif m == "conflict":
    rev, turn = lost_settle_write()
    c.registry.bind_anchor(c._rid, 2, dispatch_turn_id="a-different-turn", source="dispatch_receipt")
    out["recovered"] = c.reconciler.recover_on_start(c.adapter)
    out["gen2"] = gen2()
elif m == "stale":
    rev, turn = lost_settle_write()
    c.store.db.execute("UPDATE deliveries SET attempt_count = attempt_count + 1 WHERE event_id = ?", (rev,))
    with mock.patch.object(c.reconciler, "_is_current", return_value=True):
        out["recovered"] = c.reconciler.recover_on_start(c.adapter)
    out["gen2"] = gen2()
