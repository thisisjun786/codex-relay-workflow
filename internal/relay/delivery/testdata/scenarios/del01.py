_r, e = c.queued_event()
out["first"] = dict(c.delivery_row(e))
out["again"] = dict(c.delivery.enqueue(e))
out["eligible"] = [r["event_id"] for r in c.delivery.eligible(now=c.clock.now())]
