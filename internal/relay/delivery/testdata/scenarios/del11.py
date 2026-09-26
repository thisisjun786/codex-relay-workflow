_r, e = c.queued_event()
c.adapter.script("busy")
first = c.attempt(e)
c.clock.advance(3600)
second = c.attempt(e, now=c.clock.now())
out["first"], out["second"] = first, second
