_r, e = c.queued_event()
if sys.argv[3] == "dispatched":
    c.attempt(e)
    c.clock.advance(100000)
    out["again"] = c.attempt(e, now=c.clock.now())
else:
    c.adapter.script("turn_start_fail")
    out["first"] = c.attempt(e)
    out["rounds"] = []
    for _ in range(5):
        c.clock.advance(86400)
        out["rounds"].append([c.delivery.eligible(now=c.clock.now()), c.attempt(e, now=c.clock.now())])
