mode = sys.argv[3]
_r, e = c.queued_event(settings=None) if mode == "settings" else c.queued_event()
if mode == "stamp":
    c.adapter.threads[PARENT].archived = True
    original = c.clock.iso
    def ticking():
        c.clock.advance(0.000001)
        return original()
    c.clock.iso = ticking
else:
    if mode == "lifecycle":
        c.adapter.fail_reads("is_archived")
    elif mode == "busy":
        c.adapter.threads[PARENT].status = "active"
    original = c.adapter.read_goal_status
    def claimed(thread_id):
        c.store.db.execute("UPDATE deliveries SET state = 'sending', attempt_count = attempt_count + 1 WHERE event_id = ?", (e,))
        c.store.db.commit()
        return original(thread_id)
    c.adapter.read_goal_status = claimed
out["record"] = c.attempt(e)
