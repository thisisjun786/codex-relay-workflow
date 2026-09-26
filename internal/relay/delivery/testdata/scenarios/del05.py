_r, e = c.queued_event()
t = c.adapter.start_turn(PARENT, status="inProgress")
c.adapter.set_status(PARENT, "active")
out["record"] = c.attempt(e)
out["turn"] = c.adapter.read_turn(PARENT, t.turn_id).status
