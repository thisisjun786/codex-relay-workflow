_r, e = c.queued_event()
if len(sys.argv) > 3:
    c.adapter.start_turn(PARENT, turn_id="already-running", status="inProgress")
    c.adapter.script("steer_existing")
out["record"] = c.attempt(e)
