_r, e = c.queued_event()
c.adapter.script("busy")
out["record"] = c.attempt(e)
