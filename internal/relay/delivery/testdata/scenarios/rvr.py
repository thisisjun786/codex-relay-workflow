import json
from codex_session_relay.identity import ack_proof
from codex_session_relay.currency import head_revision
exec(open(os.path.join(os.path.dirname(__file__), "scenarios", "_vcu.py")).read())
def request_correction(e):
    result = c.ack.record_verdict(e, verdict="needs_changes", verdict_turn_id=f"review-{e}", findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix the output"}])
    g = result["nextExecutionGeneration"]
    req = c.store.one("SELECT * FROM events WHERE relationship_id = ? AND execution_generation = ? AND outcome = 'revision_request'", (c._rid, g))
    c.attempt(req["event_id"])
    bound = c.ack.bind_dispatched_revision(req["event_id"])
    c.adapter.finish_turn(CHILD, bound["dispatchTurnId"])
    return json.loads(req["receipt"])
def emit(pred, text="corrected output"):
    rel = c.registry.get(c._rid)
    g = c.registry.generation(c._rid, rel["executionGeneration"])
    turn = c.assigned_turn(turn=g["dispatchTurnId"])
    p = c.ready_payload(rel, [c.artifact("out.txt", text)], turn=turn)
    c.intake.accept_child_receipt(p, observation=turn, supersedes_revision=pred)
    return p
def ack_correction(p):
    e = p["eventId"]; c.delivery.enqueue(e); c.attempt(e); c.clock.advance(5)
    t = c.adapter.start_turn(PARENT, status="inProgress")
    c.ack.acknowledge(e, ack_turn_id=t.turn_id, ack_proof=ack_proof(e, t.turn_id), accepted=True, adapter=c.adapter)
def headof(g=2):
    return head_revision(c.store.db, c._rid, g)
m = sys.argv[3]
if m == "roundtrip":
    first = acknowledged(); req = request_correction(first); corr = emit(req["supersedesRevisionHash"]); ack_correction(corr)
    out["final"] = verdict(corr["eventId"], "verified", turn="review-corrected", findings=[{"id": "c1", "verdict": "verified"}])
    out["head"] = headof()
    out["replay"] = verdict(first, "needs_changes", turn="replayed-review")
    out["generation"] = c.registry.get(c._rid)["executionGeneration"]
elif m == "repeated":
    first = acknowledged(); req = request_correction(first)
    s = emit(req["supersedesRevisionHash"], "second output"); ack_correction(s)
    req2 = request_correction(s["eventId"]); th = emit(req2["supersedesRevisionHash"], "third output"); ack_correction(th)
    out["final"] = verdict(th["eventId"], "verified", turn="review-third", findings=[{"id": "c1", "verdict": "verified"}])
elif m == "extend":
    req = request_correction(acknowledged()); corr = emit(req["supersedesRevisionHash"], "first fix")
    latest = emit(corr["revisionHash"], "refined fix"); ack_correction(latest)
    out["final"] = verdict(latest["eventId"], "verified", turn="final-review", findings=[{"id": "c1", "verdict": "verified"}])
elif m == "historical":
    first = acknowledged(); advance(); emit(c.intake.row(first)["revision_hash"]); out["head"] = headof()
elif m == "unknown":
    request_correction(acknowledged()); emit("f" * 64); out["head"] = headof()
elif m == "earlier":
    first = acknowledged(); fh = c.intake.row(first)["revision_hash"]
    s = emit(fh, "successor"); ack_correction(s); request_correction(s["eventId"]); emit(fh); out["head"] = headof()
elif m == "other_rel":
    first = acknowledged(text="first relationship output"); fh = c.intake.row(first)["revision_hash"]
    request_correction(first)
    other = c.register(issue_key="REL-OTHER", dispatch_request_id="other-assignment", recipients=[PARENT, CHILD])
    c._rid = other["relationshipId"]
    init = emit(None, "other relationship output"); ack_correction(init); request_correction(init["eventId"]); emit(fh)
    out["head"] = headof()
elif m == "suppressed":
    req = request_correction(acknowledged())
    with c.store.transaction() as db:
        db.execute("UPDATE events SET suppressed_reason = 'withdrawn' WHERE event_id = ?", (req["eventId"],))
    emit(req["supersedesRevisionHash"]); out["head"] = headof()
elif m == "fork":
    req = request_correction(acknowledged()); emit(req["supersedesRevisionHash"], "one"); emit(req["supersedesRevisionHash"], "two")
    out["head"] = headof()
elif m == "old_unruled":
    first = acknowledged(); s = emit(c.intake.row(first)["revision_hash"], "successor"); ack_correction(s)
    req = request_correction(s["eventId"]); emit(req["supersedesRevisionHash"])
    out["r"] = verdict(first, "verified", turn="late-review")
elif m == "projection":
    first = acknowledged(); req = request_correction(first)
    out["request"] = req
    if len(sys.argv) > 4:
        out["corrected"] = emit(req["supersedesRevisionHash"])
    from codex_session_relay.assignment import AssignmentView
    proj = AssignmentView(c.store, c.registry, c.clock).state(c._rid)["projection"]
    out["projection"] = {k: proj[k] for k in ("completion", "correction")}
