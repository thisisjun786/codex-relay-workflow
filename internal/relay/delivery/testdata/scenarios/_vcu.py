from codex_session_relay import identity
from codex_session_relay.criteria import CriteriaService, set_digest
from codex_session_relay.currency import head_revision
SET = [{"id": "c1", "title": "the endpoint returns the agreed shape"}, {"id": "c2", "title": "a malformed request is refused"}]
def acknowledged(recipients=None, turn_id="ack-turn", text="the deliverable"):
    _r, e = c.queued_event(recipients=recipients or [PARENT, CHILD], text=text)
    c.attempt(e); c.clock.advance(5)
    t = c.adapter.start_turn(PARENT, turn_id=turn_id, status="inProgress")
    c.ack.acknowledge(e, ack_turn_id=t.turn_id, ack_proof=identity.ack_proof(e, t.turn_id), accepted=True, adapter=c.adapter)
    return e
def advance():
    return c.registry.open_generation(c._rid, dispatch_request_id="newer-execution", reason="needs_changes_revision", dispatch_turn_id="newer-turn")
def second(text="a different revision"):
    return c.ready_event(register=False, text=text)[1]
def verdict(e, v, turn="v1", **kw):
    return refusal(c.ack.record_verdict, e, verdict=v, verdict_turn_id=turn, **kw)
def supersede(e, text, predecessor):
    p = c.ready_payload(c.registry.get(c._rid), [c.artifact("out.txt", text)])
    c.intake.accept_child_receipt(p, observation=c.assigned_turn(), supersedes_revision=predecessor)
    return p
crit = CriteriaService(c.store, c.clock)
def register_criteria(entries=None):
    return crit.register(c._rid, entries or SET, source_ref="https://linear.app/doc/1")
def head(g=1):
    return head_revision(c.store.db, c._rid, g)
