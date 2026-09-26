from codex_session_relay import identity
rel, e = c.queued_event()
out["delivered"] = c.attempt(e)
c.clock.advance(5)
t = c.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
out["claim"] = c.ack.claim_verification(e, turn_id="ack-turn")
out["ack"] = c.ack.acknowledge(e, ack_turn_id=t.turn_id, ack_proof=identity.ack_proof(e, t.turn_id), accepted=True, adapter=c.adapter)
out["verdict"] = c.ack.record_verdict(e, verdict="verified", verdict_turn_id="verdict-1")
out["duplicate"] = refusal(c.accept, c.ready_payload(rel, [c.artifact("out.txt", "the deliverable")]))
