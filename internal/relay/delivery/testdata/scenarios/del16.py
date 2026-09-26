_r, e = c.queued_event()
t = c.adapter.threads[PARENT]
if sys.argv[3] == "paused":
    t.goal_status = "paused"; c.attempt(e); t.goal_status = "active"
else:
    t.archived = True; c.attempt(e); t.archived = False
c.clock.advance(c.delivery.policy.lifecycle_recheck_seconds + 1)
out["eligible"] = [r["event_id"] for r in c.delivery.eligible(now=c.clock.now())]
out["record"] = c.attempt(e, now=c.clock.now())
