_r, e = c.queued_event()
t = c.adapter.threads[PARENT]
mode = sys.argv[3]
if mode == "archived": t.archived = True
elif mode == "paused": t.goal_status = "paused"
elif mode == "budget": t.goal_status = "budgetLimited"
elif mode == "noinput": t.can_accept_input = False
elif mode == "idle": t.archived = True; t.status = "idle"
elif mode == "unreadable": c.adapter.fail_reads("read_goal_status")
out["status"] = c.registry.get(c._rid)["status"]
out["record"] = c.attempt(e)
