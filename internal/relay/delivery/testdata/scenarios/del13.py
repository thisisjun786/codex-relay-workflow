_r, e = c.queued_event()
mode = sys.argv[3]
if mode == "direct":
    out["records"] = []
    for _ in range(c.delivery.policy.max_attempts):
        c.adapter.script("read_fail"); c.clock.advance(100000)
        out["records"].append(c.attempt(e, now=c.clock.now()))
    c.clock.advance(100000)
    out["eligible"] = c.delivery.eligible(now=c.clock.now())
elif mode == "reconciled":
    out["outcomes"] = []
    for _ in range(c.delivery.policy.max_attempts):
        c.adapter.script("in_progress"); c.clock.advance(100000)
        record = c.attempt(e, now=c.clock.now())
        if record is None:
            break
        c.adapter.ledger[record["requestId"]] = {"requestId": record["requestId"], "status": "failed", "error": "thread/read: refused", "rpcError": {"code": "internal", "message": "refused"}}
        out["outcomes"].append(c.reconciler.reconcile_attempt(record["requestId"], c.adapter, now=c.clock.now()))
elif mode == "interval":
    c.adapter.script("read_fail")
    c.attempt(e)
    c.delivery._reschedule(e, "withheld_pre_send", c.clock.now(), attempts=c.delivery_row(e)["attempt_count"])
    out["again"] = c.attempt(e, now=c.clock.now() + 1)
else:
    now = c.clock.now()
    with c.store.transaction() as db:
        db.execute("INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,?,?)", (PARENT, int(now // 3600) * 3600, c.delivery.policy.max_sends_per_recipient_per_hour, 0))
    out["again"] = c.attempt(e, now=now)
