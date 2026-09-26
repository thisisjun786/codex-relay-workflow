_r, e = c.queued_event()
rid = c._rid
with c.store.transaction() as db:
    out["first"] = c.registry.open_generation_in(db, rid, dispatch_request_id="revision-x", reason="needs_changes_revision")
with c.store.transaction() as db:
    out["replay"] = c.registry.open_generation_in(db, rid, dispatch_request_id="revision-x", reason="needs_changes_revision")
with c.store.transaction() as db:
    out["next"] = c.registry.open_generation_in(db, rid, dispatch_request_id="revision-y", reason="needs_changes_revision")
