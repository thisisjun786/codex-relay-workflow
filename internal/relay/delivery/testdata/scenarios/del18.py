_r, e = c.queued_event()
c.registry.set_status(c._rid, sys.argv[3], actor="user")
out["record"] = c.attempt(e)
