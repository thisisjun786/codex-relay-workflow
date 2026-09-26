"""Record the Python AssignmentView's answers for the ASG-* parity tests.

  uv run --no-sync python internal/relay/registry/testdata/gen_assignment.py \
      > internal/relay/registry/testdata/python_assignment.json

Each scenario drives the real Python services through tests/test_assignment.py's own fixtures
(FakeClock, FakeHostAdapter) and records checkpoints. A checkpoint is the whole store as SQL
INSERT statements at that moment, the clock, the call made and Python's whole answer. The Go test
loads the same rows into a Go store, makes the same call and compares the whole JSON.
"""
import json
import os
import sys

sys.path.insert(0, "packages/codex-session-relay")

from codex_session_relay.errors import RelayError  # noqa: E402
from codex_session_relay.models import Endpoint  # noqa: E402
from codex_session_relay.registry import contract_record  # noqa: E402
from tests import test_assignment as ta  # noqa: E402
from tests.support import CHILD, HOST, ISSUE, PARENT  # noqa: E402

SCENARIOS = {}


def scenario(cls=ta.AssignmentTestCase):
    def wrap(fn):
        SCENARIOS[fn.__name__] = (cls, fn)
        return fn
    return wrap


def dump(tc):
    lines = []
    for line in tc.store.db.iterdump():
        if line.startswith("INSERT INTO") or line.startswith('DELETE FROM "sqlite_sequence"'):
            lines.append(line.replace("INSERT INTO", "INSERT OR REPLACE INTO", 1))
    return "\n".join(lines)


def outcome(call):
    try:
        value = call()
    except RelayError as error:
        return {"refused": {"reason": error.reason.value if error.reason else None,
                            "detail": error.detail}}
    return {"ok": json.loads(json.dumps(value, default=str))}


class Recorder:
    def __init__(self, tc):
        self.tc = tc
        self.points = []

    def point(self, op, **args):
        tc = self.tc
        sql = dump(tc)
        if op == "state":
            call = lambda: tc.assignments.state(args["relationship"])  # noqa: E731
        elif op == "for_issue":
            call = lambda: tc.assignments.for_issue(args["issue"])  # noqa: E731
        elif op == "mark":
            call = lambda: tc.assignments.mark(  # noqa: E731
                args["relationship"], "merged", evidence=args["evidence"], actor=args["actor"],
                expected_event=args["expected_event"])
        elif op == "register":
            def call():
                return contract_record(tc.registry.register(
                    parent=Endpoint(PARENT, HOST, cwd="/parent"),
                    child=Endpoint(args["child"], HOST, cwd=tc.root),
                    issue_key=args["issue"], artifact_roots=[tc.root],
                    allowed_recipients=[PARENT],
                    dispatch_request_id=args["dispatch"], dispatch_turn_id=args["turn"],
                    supersedes=args.get("supersedes")))
        else:
            raise ValueError(op)
        result = outcome(call)
        self.points.append({"op": op, "args": args, "sql": sql, "now": tc.clock.now(),
                            "iso": tc.clock.iso(), "stateDir": os.path.dirname(str(tc.store.path)),
                            "root": tc.root, "result": result})
        return result


def rid(tc):
    return tc._rid


@scenario()
def requested(tc, rec):
    tc._rid = tc.register()["relationshipId"]
    rec.point("state", relationship=rid(tc))


@scenario()
def received(tc, rec):
    tc.ready_event()
    rec.point("state", relationship=rid(tc))


@scenario()
def verifying(tc, rec):
    _r, event_id = tc.ready_event()
    tc.ack.claim_verification(event_id, turn_id="ack-turn")
    rec.point("state", relationship=rid(tc))


@scenario()
def verified(tc, rec):
    tc.verified_head()
    rec.point("state", relationship=rid(tc))


@scenario()
def needs_changes_then_corrected(tc, rec):
    from codex_session_relay import identity
    _relationship, event_id = tc.queued_event(recipients=[PARENT, CHILD])
    tc.attempt(event_id)
    tc.clock.advance(5)
    turn = tc.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
    tc.ack.acknowledge(event_id, ack_turn_id=turn.turn_id,
                       ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
                       adapter=tc.adapter)
    tc.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="v1",
                          findings=[{"id": "c1", "verdict": "needs_changes", "note": "fix the shape"}])
    first = rec.point("state", relationship=rid(tc))
    correction = first["ok"]["projection"]["correction"]["eventId"]
    tc.clock.advance(1)
    tc.attempt(correction)
    rec.point("state", relationship=rid(tc))
    relationship = tc.registry.get(rid(tc))
    generation = relationship["executionGeneration"]
    tc.registry.bind_anchor(rid(tc), generation, dispatch_turn_id="revision-turn",
                            source="dispatch_receipt")
    path = tc.artifact("out.txt", "the corrected revision")
    payload = tc.ready_payload(relationship, [path], generation=generation,
                               turn=tc.assigned_turn(turn="revision-turn"))
    tc.intake.accept_child_receipt(payload, observation=tc.assigned_turn(turn="revision-turn"))
    rec.point("state", relationship=rid(tc))


@scenario()
def ambiguous(tc, rec):
    tc.verified_head()
    tc.ready_event(register=False, text="a competing revision")
    rec.point("state", relationship=rid(tc))


@scenario()
def paused(tc, rec):
    tc.verified_head()
    tc.registry.set_status(rid(tc), "paused", actor=PARENT)
    rec.point("state", relationship=rid(tc))


@scenario()
def paused_after_mark(tc, rec):
    event_id = tc.verified_head()
    tc.assignments.mark(rid(tc), "merged", evidence="merged as abc1234", actor=PARENT,
                        expected_event=event_id)
    tc.registry.set_status(rid(tc), "paused", actor=PARENT)
    rec.point("state", relationship=rid(tc))


@scenario()
def mark_merged(tc, rec):
    event_id = tc.verified_head()
    rec.point("mark", relationship=rid(tc), evidence="merged into dev as abc1234", actor=PARENT,
              expected_event=event_id)


@scenario()
def mark_unverified(tc, rec):
    _r, event_id = tc.ready_event()
    rec.point("mark", relationship=rid(tc), evidence="merged anyway", actor=PARENT,
              expected_event=event_id)


@scenario()
def mark_then_new_generation(tc, rec):
    event_id = tc.verified_head()
    tc.assignments.mark(rid(tc), "merged", evidence="merged as abc1234", actor=PARENT,
                        expected_event=event_id)
    tc.registry.open_generation(rid(tc), dispatch_request_id="newer",
                                reason="needs_changes_revision", dispatch_turn_id="newer-turn")
    rec.point("state", relationship=rid(tc))


@scenario()
def mark_then_new_revision(tc, rec):
    event_id = tc.verified_head()
    tc.assignments.mark(rid(tc), "merged", evidence="merged as abc1234", actor=PARENT,
                        expected_event=event_id)
    superseded = tc.intake.row(event_id)["revision_hash"]
    path = tc.artifact("out.txt", "a newer revision after the merge")
    payload = tc.ready_payload(tc.registry.get(rid(tc)), [path])
    tc.intake.accept_child_receipt(payload, observation=tc.assigned_turn(),
                                   supersedes_revision=superseded)
    rec.point("state", relationship=rid(tc))


@scenario()
def mark_stale(tc, rec):
    from codex_session_relay import identity
    first = tc.verified_head()
    superseded = tc.intake.row(first)["revision_hash"]
    path = tc.artifact("out.txt", "a newer revision")
    payload = tc.ready_payload(tc.registry.get(rid(tc)), [path])
    tc.intake.accept_child_receipt(payload, observation=tc.assigned_turn(),
                                   supersedes_revision=superseded)
    tc.delivery.enqueue(payload["eventId"])
    tc.attempt(payload["eventId"])
    tc.clock.advance(5)
    turn = tc.adapter.start_turn(PARENT, turn_id="ack-turn-2", status="inProgress")
    tc.ack.acknowledge(payload["eventId"], ack_turn_id=turn.turn_id,
                       ack_proof=identity.ack_proof(payload["eventId"], turn.turn_id),
                       accepted=True, adapter=tc.adapter)
    tc.ack.record_verdict(payload["eventId"], verdict="verified", verdict_turn_id="v2")
    rec.point("mark", relationship=rid(tc), evidence="integrated the first revision",
              actor=PARENT, expected_event=first)


@scenario()
def mark_without_expected_event(tc, rec):
    tc.verified_head()
    rec.point("mark", relationship=rid(tc), evidence="merged", actor=PARENT, expected_event="")


@scenario(ta.CriteriaCurrency)
def criteria_edited(tc, rec):
    event_id = tc.managed_verified()
    rec.point("state", relationship=rid(tc))
    tc.edit_criteria()
    rec.point("state", relationship=rid(tc))
    rec.point("mark", relationship=rid(tc), evidence="merged as abc1234", actor=PARENT,
              expected_event=event_id)


@scenario(ta.CriteriaCurrency)
def criteria_edited_after_mark(tc, rec):
    event_id = tc.managed_verified()
    tc.assignments.mark(rid(tc), "merged", evidence="merged as abc1234", actor=PARENT,
                        expected_event=event_id)
    tc.edit_criteria()
    rec.point("state", relationship=rid(tc))


def rival(rec, **kw):
    args = {"child": ta.OTHER_CHILD, "issue": ISSUE, "dispatch": "rival-dispatch", "turn": "rival-turn"}
    args.update(kw)
    return rec.point("register", **args)


@scenario(ta.DuplicateAssignment)
def duplicate_active(tc, rec):
    tc.register()
    rival(rec)


@scenario(ta.DuplicateAssignment)
def duplicate_paused(tc, rec):
    relationship = tc.register()
    tc.registry.set_status(relationship["relationshipId"], "paused", actor=PARENT)
    rival(rec)


@scenario(ta.DuplicateAssignment)
def duplicate_archived(tc, rec):
    relationship = tc.register()
    tc.registry.set_status(relationship["relationshipId"], "archived", actor=PARENT)
    rival(rec)


@scenario(ta.DuplicateAssignment)
def duplicate_same_pair(tc, rec):
    tc.register()
    rec.point("register", child=CHILD, issue=ISSUE, dispatch="dispatch-1", turn="turn-dispatch-1")


@scenario(ta.DuplicateAssignment)
def duplicate_supersedes(tc, rec):
    relationship = tc.register()
    rival(rec, supersedes=relationship["relationshipId"])
    tc._rid = relationship["relationshipId"]


@scenario()
def for_issue_owner(tc, rec):
    rec.point("for_issue", issue=ISSUE)
    rec.point("for_issue", issue="NOT-AN-ISSUE")
    tc.register()
    rec.point("for_issue", issue=ISSUE)
    rec.point("for_issue", issue="NOT-AN-ISSUE")


@scenario()
def for_issue_paused(tc, rec):
    relationship = tc.register()
    tc.registry.set_status(relationship["relationshipId"], "paused", actor=PARENT)
    rec.point("for_issue", issue=ISSUE)


def patched(cls, method, reader):
    """Run a Python test method as written, recording a checkpoint at every read it makes."""
    def fn(tc, rec):
        if reader == "read":
            def read():
                record = rec.point("state", relationship=tc._rid)["ok"]
                return record, record["projection"]["correction"]
            tc.read = read
        else:
            tc.projection = lambda: rec.point("state", relationship=tc._rid)["ok"]["projection"]
        getattr(tc, method)()
    SCENARIOS[method] = (cls, fn)


for method in ("test_a_staged_event_has_no_delivery_at_all",
               "test_the_axes_keep_their_own_vocabulary_through_the_lifecycle",
               "test_the_request_id_names_the_current_attempt_not_the_first",
               "test_a_generation_with_no_correction_answers_null_not_a_borrowed_row",
               "test_a_verified_rejection_does_not_read_like_a_verified_acceptance",
               "test_a_refusal_stops_being_the_reason_once_a_delivery_exists"):
    patched(ta.TheAxesStayApart, method, "projection")
for method in [m for m in dir(ta.UnsentCorrection) if m.startswith("test_")
               and m != "test_the_correction_is_still_read_in_one_statement"]:
    patched(ta.UnsentCorrection, method, "read")


@scenario()
def verdict_referenced(tc, rec):
    tc.queued_event(recipients=[PARENT, CHILD])
    rec.point("state", relationship=rid(tc))


out = {}
for name, (cls, fn) in SCENARIOS.items():
    tc = type("Run", (cls,), {"runTest": lambda self: None})("runTest")
    tc.setUp()
    try:
        rec = Recorder(tc)
        fn(tc, rec)
        out[name] = rec.points
    finally:
        tc.doCleanups()
from codex_session_relay.supervisorchannel import relay_program  # noqa: E402
out["__program__"] = [{"op": "program", "args": {}, "sql": "", "now": 0, "iso": "", "stateDir": "",
                       "root": "", "result": {"ok": list(relay_program())}}]
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
