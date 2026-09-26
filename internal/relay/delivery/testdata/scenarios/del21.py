_r, e = c.queued_event()
mode = sys.argv[3]
if mode in ("running", "shorten"):
    c.adapter.set_status(PARENT, "active")
    out["first"] = c.attempt(e)
    out["deferred_until"] = c.delivery_row(e)["next_eligible_at"]
    c.registry.set_status(c._rid, "cancelled", actor="user")
    out["record"] = c.attempt(e)
    out["after"] = c.delivery_row(e)["next_eligible_at"]
else:
    c.registry.set_status(c._rid, "cancelled", actor="user")
    stale = c.registry.get(c._rid)
    far = c.clock.now() + 100000
    with c.store.transaction() as db:
        db.execute("UPDATE deliveries SET next_eligible_at = ? WHERE event_id = ?", (far, e))
    out["record"] = c.delivery._withhold_inactive(e, stale, c.clock.now(), attempts=0)
    out["after"] = c.delivery_row(e)["next_eligible_at"]
