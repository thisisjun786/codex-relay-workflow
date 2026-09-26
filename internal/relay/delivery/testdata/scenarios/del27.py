from codex_session_relay import identity
mode = sys.argv[3]
if mode == "completion":
    _r, e = c.queued_event()
    out["message"] = c.delivery.render_message(e)
else:
    _r, e = c.queued_event(recipients=[PARENT, CHILD])
    c.attempt(e)
    c.clock.advance(5)
    turn = c.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
    out["ack"] = c.ack.acknowledge(e, ack_turn_id=turn.turn_id, ack_proof=identity.ack_proof(e, turn.turn_id), accepted=True, adapter=c.adapter)
    out["verdict"] = c.ack.record_verdict(e, verdict="needs_changes", verdict_turn_id="verdict-1", criteria=[{"id": "c-1", "verdict": "needs_changes", "note": "the manifest omits the migration script"}])
    rev = c.store.one("SELECT * FROM deliveries WHERE kind = 'revision_request'")["event_id"]
    out["revision"] = rev
    out["message"] = c.delivery.render_message(rev)
    out["childAck"] = refusal(c.ack.acknowledge, rev, ack_turn_id="child-turn", ack_proof=identity.ack_proof(rev, "child-turn"), accepted=True, adapter=c.adapter)
    out["generation"] = c.registry.get(c._rid)["executionGeneration"]
