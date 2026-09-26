_r, e = c.queued_event()
out["preview"] = c.delivery.render_message(e)
out["record"] = c.attempt(e)
