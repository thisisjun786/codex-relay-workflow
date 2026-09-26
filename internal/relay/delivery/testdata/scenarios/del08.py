_r, e = c.queued_event()
c.attempt(e)
s = c.delivery.snapshot()["deliveries"][0]
out["snapshot"] = {k: s[k] for k in ("state", "reported", "acknowledged", "phase")}
