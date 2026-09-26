from codex_session_relay import identity
def dispatched():
    _r, e = c.queued_event(recipients=[PARENT, CHILD]); c.attempt(e); c.clock.advance(5); return e
def advance():
    return c.registry.open_generation(c._rid, dispatch_request_id="newer-execution", reason="needs_changes_revision", dispatch_turn_id="newer-turn")
m = sys.argv[3]
e = dispatched()
if m in ("offline", "no_verdict"):
    out["ack"] = c.ack.acknowledge(e, ack_turn_id="parent-own-turn", ack_proof=identity.ack_proof(e, "parent-own-turn"), accepted=True, adapter=None)
    if m == "no_verdict":
        out["r"] = refusal(c.ack.record_verdict, e, verdict="verified", verdict_turn_id="v1")
elif m == "dispatch_turn":
    t = c.delivery.find(e)["dispatch_turn_id"]
    out["ack"] = c.ack.acknowledge(e, ack_turn_id=t, ack_proof=identity.ack_proof(e, t), accepted=True, adapter=None)
else:
    c.adapter.start_turn(PARENT, turn_id="parent-own-turn", status="inProgress")
    out["ack"] = c.ack.acknowledge(e, ack_turn_id="parent-own-turn", ack_proof=identity.ack_proof(e, "parent-own-turn"), accepted=True, adapter=None)
    if m == "upgrade":
        out["r"] = c.ack.verify_pending_acks(c.adapter)
    elif m == "advanced":
        advance(); out["r"] = c.ack.verify_pending_acks(c.adapter)
    elif m == "paused":
        c.registry.set_status(c._rid, "paused", actor=PARENT); out["r"] = c.ack.verify_pending_acks(c.adapter)
    elif m == "twice":
        advance()
        out["r"] = c.ack.verify_pending_acks(c.adapter, now=c.clock.now())
        out["r2"] = c.ack.verify_pending_acks(c.adapter, now=c.clock.now() + 10000)
    elif m == "next_check":
        advance()
        out["r"] = c.ack.verify_pending_acks(c.adapter, now=100.0)
        out["r2"] = c.ack.verify_pending_acks(c.adapter, now=101.0)
    elif m == "cleared":
        c.registry.set_status(c._rid, "paused", actor=PARENT)
        out["r"] = c.ack.verify_pending_acks(c.adapter, now=100.0)
        rel = c.registry.get(c._rid)
        c.registry.resume(c._rid, expect_generation=rel["executionGeneration"], expect_artifact_roots=rel["authorizedScope"]["artifactRoots"], expect_allowed_recipients=rel["authorizedScope"]["allowedRecipients"], actor=PARENT)
        out["r2"] = c.ack.verify_pending_acks(c.adapter, now=100000.0)
