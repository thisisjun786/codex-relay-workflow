_r, e = c.queued_event()
mode = sys.argv[3]
if mode == "before":
    c.registry.set_status(c._rid, "paused", actor="user")
    out["eligible"] = c.delivery.eligible(now=c.clock.now())
else:
    original = c.adapter.read_goal_status
    def hook(thread_id):
        if mode == "supersede":
            c.registry.supersede(c._rid, new_relationship_id="rel-bbbbbbbbbbbbbbbb")
        else:
            c.registry.set_status(c._rid, mode, actor="user")
        return original(thread_id)
    c.adapter.read_goal_status = hook
    out["record"] = c.attempt(e)
