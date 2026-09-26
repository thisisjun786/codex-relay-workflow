_r, e = c.queued_event()
c.adapter.threads[PARENT].approval_policy = sys.argv[3]
c.adapter.script("approval_policy")
out["record"] = c.attempt(e)
s = c.delivery.snapshot()["deliveries"][0]
out["snapshot"] = {k: s[k] for k in ("state", "reported", "holdReason", "phase")}
c.clock.advance(100000)
out["eligible"] = c.delivery.eligible(now=c.clock.now())
