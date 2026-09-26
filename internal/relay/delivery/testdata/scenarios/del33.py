_r, e = c.queued_event()
now = c.clock.now()
c.store.db.execute("INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,1,?)", (PARENT, int(now // 3600) * 3600, now))
c.store.db.commit()
c.delivery._rate_limited = lambda recipient, at: False
out["first"] = c.attempt(e, now=now + 1)
out["row"] = dict(c.delivery_row(e))
del c.delivery._rate_limited
out["record"] = c.attempt(e, now=out["row"]["next_eligible_at"])
