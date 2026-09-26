from codex_session_relay.models import TurnRef
def advance(number):
    t = f"turn-dispatch-{number}"
    c.adapter.start_turn(CHILD, turn_id=t, status="inProgress")
    return c.registry.open_generation(c._rid, dispatch_request_id=f"dispatch-{number}", reason="needs_changes_revision", dispatch_turn_id=t)
def queued_outcome(outcome):
    rel = c.register(); c._rid = rel["relationshipId"]
    p = c.ready_payload(rel, [c.artifact("out.txt", "the deliverable")]) if outcome == "ready_for_review" else c.execution_payload(rel, outcome)
    c.accept(p); c.delivery.enqueue(p["eventId"])
    return rel, p["eventId"]
def declare(successor, older):
    with c.store.transaction() as db:
        db.execute("UPDATE revision_lineage SET supersedes_hash = ? WHERE event_id = ?", (c.intake.get(older)["revisionHash"], successor))
def item(e):
    s = [d for d in c.delivery.snapshot()["deliveries"] if d["eventId"] == e][0]
    return {k: s[k] for k in ("state", "reported", "phase", "supersededNote", "holdReason")}
m = sys.argv[3]
if m in ("queued", "outstanding", "capped", "withheld_cap", "dispatched", "current", "mark"):
    rel, e = queued_outcome("ready_for_review")
    if m in ("outstanding", "current", "mark"):
        c.adapter.script("transport_unknown")
        if m == "mark": c.clock.advance(3600)
        c.attempt(e)
    elif m == "capped":
        with c.store.transaction() as db: db.execute("UPDATE deliveries SET state = ?, hold_reason = ? WHERE event_id = ?", ("deferred_busy", "busy_cap", e))
    elif m == "withheld_cap":
        with c.store.transaction() as db: db.execute("UPDATE deliveries SET state = ?, hold_reason = ? WHERE event_id = ?", ("withheld_pre_send", "presend_cap", e))
    elif m == "dispatched":
        c.attempt(e)
    advance(2)
    if m == "current":
        c.store.db.execute("DELETE FROM delivery_supersession WHERE event_id = ?", (e,))
        advance(3)
    if m == "mark":
        c.delivery.mark_superseded(e, reason="stale_generation")
    out["item"] = item(e)
elif m == "terminal" or m == "queued_pred":
    rel, older = queued_outcome("ready_for_review")
    if m == "terminal":
        c.adapter.script("transport_unknown"); c.attempt(older)
    s = c.ready_payload(rel, [c.artifact("newer.txt", "the corrected deliverable")], attempt=2)
    c.accept(s); declare(s["eventId"], older)
    c.delivery.enqueue(s["eventId"])
    out["item"] = item(older)
elif m == "outstanding_pred":
    rel, older = queued_outcome("ready_for_review")
    c.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
    c.adapter.script("transport_unknown"); c.attempt(older)
    s = c.ready_payload(rel, [c.artifact("newer.txt", "the corrected deliverable")], attempt=2, turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"))
    c.accept(s); declare(s["eventId"], older)
    c.intake.resolve_staged(TurnRef(CHILD, "turn-dispatch-1", "completed"))
    c.delivery.annotate_predecessors(s["eventId"])
    out["item"] = item(older)
elif m == "reemit":
    rel = c.register()
    p = c.ready_payload(rel, [c.artifact("out.txt", "the deliverable")])
    out["first"] = c.accept(p); out["again"] = c.accept(p)
elif m == "exec_only":
    rel, reviewable = queued_outcome("ready_for_review")
    c.attempt(reviewable)
    later = c.execution_payload(rel, "failed")
    c.accept(later); c.delivery.enqueue(later["eventId"])
    with c.store.transaction() as db:
        out["reason"] = c.delivery._supersession_reason(db, later["eventId"])
    out["state"] = c.delivery.get(later["eventId"])["state"]
elif m in ("intent", "stranded"):
    rel, e = queued_outcome("ready_for_review")
    if m == "intent":
        c.store.db.execute("DELETE FROM deliveries WHERE event_id = ?", (e,))
    with c.store.transaction() as db:
        c.delivery.record_intent_in(db, e, relationship_id=rel["relationshipId"], kind="completion", recipient_task_id=PARENT, error=RuntimeError("the relationship was paused"), now=c.clock.now())
    out["row"] = dict(c.delivery.enqueue(e))
elif m.startswith("presend_"):
    outcome = m[len("presend_"):]
    rel, e = queued_outcome(outcome)
    advance(2); c.clock.advance(3600)
    out["record"] = c.attempt(e)
elif m == "busy_release":
    rel, e = queued_outcome("ready_for_review")
    c.adapter.set_status(PARENT, "active")
    out["first"] = c.attempt(e)
    advance(2); c.adapter.set_status(PARENT, "idle"); c.clock.advance(3600)
    out["record"] = c.attempt(e)
elif m == "newer":
    rel, older = queued_outcome("ready_for_review")
    n = c.ready_payload(rel, [c.artifact("newer.txt", "the corrected deliverable")], attempt=2)
    c.accept(n); declare(n["eventId"], older)
    c.delivery.enqueue(n["eventId"]); c.clock.advance(3600)
    out["record"] = c.attempt(older); c.clock.advance(3600)
    out["sent"] = c.attempt(n["eventId"])
elif m == "staged_successor":
    rel, older = queued_outcome("ready_for_review")
    st = c.ready_payload(rel, [c.artifact("staged.txt", "still being written")], attempt=2, turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"))
    c.accept(st); c.clock.advance(3600)
    out["record"] = c.attempt(older)
