_r, e = c.queued_event()
if sys.argv[3] == "superseded":
    c.registry.supersede(c._rid, new_relationship_id="rel-bbbbbbbbbbbbbbbb")
    out["record"] = c.attempt(e)
else:
    stale = dict(c.registry.get(c._rid)); stale["status"] = "paused"
    out["record"] = c.delivery._withhold_inactive(e, stale, c.clock.now(), attempts=0)
