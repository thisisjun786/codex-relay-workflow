"""Build the test_diagnostics.py scenarios with the real Python relay, for the Go comparison.

Run from packages/codex-session-relay (so `tests` imports) with one argument: an empty directory.
Each scenario runs the Python test's own steps - the test method itself where it drives the real
DeliveryService/daemon, or seeded rows where the Python test calls _phase directly - leaves its
store on disk, then records Python's `status` output for that store at the scenario's FakeClock
instant. Prints one JSON document: {scenario: {state, now, argv, stdout, program}}.
"""

import contextlib
import io
import json
import os
import sys
import tempfile
from unittest import mock

from codex_session_relay import cli
from codex_session_relay.clock import SystemClock
from codex_session_relay.supervisorchannel import relay_program

from tests import test_diagnostics as td
from tests.support import CHILD

ROOT = sys.argv[1]
tempfile.tempdir = ROOT
results = {}


def python_status(state, now, argv):
    out = io.StringIO()
    with mock.patch.object(SystemClock, "now", lambda self: now), contextlib.redirect_stdout(out):
        code = cli.main(["--state", state, "status", *argv])
    assert code == 0, (code, out.getvalue())
    return out.getvalue()


def record(name, case, argv=()):
    state = os.path.dirname(str(case.store.path))
    now = case.clock.now()
    results[name] = {"state": state, "now": now, "argv": list(argv),
                     "stdout": python_status(state, now, argv),
                     "program": " ".join(relay_program())}


def run(cls, method, name=None, argv=()):
    """The Python test itself, its assertions included, then its store is kept."""
    case = cls(method)
    case.setUp()
    getattr(case, method)()
    record(name or method, case, argv)
    return case


def seeded(name, *, state, kind="completion_event", hold_reason=None, record_json=None,
           failure=None, ack=None):
    """A queued delivery rewritten to the row _phase is called with in the Python test."""
    case = td.Phases("test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt")
    case.setUp()
    _relationship, event_id = case.queued_event()
    db = case.store.db
    db.execute("UPDATE deliveries SET state = ?, kind = ?, hold_reason = ? WHERE event_id = ?",
               (state, kind, hold_reason, event_id))
    if record_json is not None:
        db.execute("INSERT INTO attempts (request_id, event_id, attempt_no, kind,"
                   " internal_state, state, record, observed_at) VALUES (?,?,1,?,?,?,?,?)",
                   (f"{event_id}:1", event_id, kind, "settled", state,
                    json.dumps(record_json), case.clock.iso()))
    if failure is not None:
        db.execute("INSERT INTO failed_operations (scope_key, operation, detail, error_code,"
                   " occurred_at) VALUES (?,?,?,?,?)",
                   (event_id, failure["operation"], "seeded", failure["error_code"],
                    case.clock.iso()))
    if ack is not None:
        db.execute("INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at)"
                   " VALUES (?,?,?,?,?,?)",
                   (event_id, "{}", "ack-turn", ack["accepted"], ack["verified"],
                    case.clock.iso()))
    record(name, case)


# Phases and NoAttemptPhases: the real delivery path.
for method in ("test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt",
               "test_a_busy_parent_is_named_as_such_with_its_next_retry",
               "test_a_dispatched_delivery_is_awaiting_acknowledgement",
               "test_a_closed_channel_is_queryable_rather_than_hidden",
               "test_a_settings_rejection_records_the_field_the_host_disagreed_on",
               "test_the_diagnosis_survives_reopening_the_database"):
    run(td.Phases, method)
run(td.NoAttemptPhases, "test_missing_settings_are_named_rather_than_reported_as_awaiting_a_receipt")

# The _phase direct calls, as the rows they describe.
seeded("test_a_delivery_whose_send_is_in_flight_says_so", state="sending")
seeded("test_a_refused_turn_start_is_not_evidence_that_a_turn_exists", state="held_uncertain",
       record_json={"failedOperation": "turn/start", "turnId": None})
seeded("test_a_started_turn_whose_answer_was_lost_is_still_turn_accepted", state="held_uncertain",
       record_json={"failedOperation": "turn/start", "turnId": "turn-9"})
seeded("test_an_ordinary_resume_failure_is_not_called_a_settings_rejection",
       state="withheld_pre_send", record_json={"failedOperation": "thread/resume"},
       failure={"operation": "transport", "error_code": "internal"})
seeded("test_a_real_settings_rejection_still_says_so", state="withheld_pre_send",
       record_json={"failedOperation": "thread/resume"},
       failure={"operation": "settings_check", "error_code": "settings_not_preserved"})
seeded("test_a_verified_rejection_is_not_reported_as_awaiting_a_receipt", state="dispatched",
       ack={"verified": "verified", "accepted": 0})
seeded("test_a_verified_acceptance_still_reports_acknowledged", state="dispatched",
       ack={"verified": "verified", "accepted": 1})
seeded("test_an_unverified_acknowledgement_settles_nothing", state="dispatched",
       ack={"verified": "unverified", "accepted": 1})

run(td.RefusedBeforeTheQueue, "test_an_event_refused_at_the_queue_is_visible_in_status")
run(td.RefusedBeforeTheQueue, "test_a_scoped_status_filters_the_pending_intents_too",
    argv=("--relationship", "rel-someone-else"))
run(td.RevisionPhases, "test_a_dispatched_revision_is_not_waiting_for_an_acknowledgement")
run(td.RevisionPhases, "test_a_dispatched_completion_still_awaits_its_acknowledgement")

for method in ("test_a_live_loop_with_nothing_polled_is_not_healthy",
               "test_a_successful_poll_then_a_failure_keeps_the_last_success",
               "test_a_staged_backlog_is_visible_with_its_age",
               "test_an_anchor_the_scheduler_has_not_reached_is_not_reported_healthy",
               "test_a_finished_quiet_assignment_does_not_age_into_a_false_alarm",
               "test_a_late_staged_receipt_reopens_the_same_anchor",
               "test_each_assignment_settles_a_shared_turn_for_itself",
               "test_another_assignments_staged_work_does_not_unsettle_this_one",
               "test_a_cancelled_assignments_staged_event_does_not_hold_health_down",
               "test_backlog_and_staged_events_agree_after_an_assignment_is_cancelled",
               "test_a_generation_whose_anchor_is_not_bound_yet_is_not_a_stall",
               "test_an_unbound_anchor_becomes_pollable_once_it_binds"):
    run(td.ObservationHealth, method)

# The upgrade: the test's own steps up to the point a new build opens the store.
case = td.ObservationHealth("test_an_upgraded_store_does_not_forget_what_it_had_already_settled")
case.setUp()
relationship = case.register()
case.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
case.adapter.finish_turn(CHILD, "turn-dispatch-1")
case.daemon.tick(now=case.clock.now())
assert case.store.all("SELECT * FROM assignment_settlements")
with case.store.transaction() as db:
    db.execute("DELETE FROM assignment_settlements")
case.store.close()
results["test_an_upgraded_store_does_not_forget_what_it_had_already_settled"] = {
    "state": os.path.dirname(str(case.store.path)), "now": case.clock.now(), "argv": [],
    "stdout": "", "program": " ".join(relay_program()),
    "relationshipId": relationship["relationshipId"]}



def python_show(state, argv):
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = cli.main(["--state", state, "show", *argv])
    return {"code": code, "stdout": out.getvalue()}


# show over every event a scenario left, with --message where an attempt froze its bytes (the
# unsent preview needs the delivery renderer, which is todo 21's).
shows = {}
for name, found in results.items():
    if not found["stdout"]:
        continue
    for item in json.loads(found["stdout"])["deliveries"]:
        argv = ["--event", item["eventId"]]
        shows[f"{name}/{item['eventId']}"] = {"state": found["state"], "argv": argv,
                                              **python_show(found["state"], argv)}
        # --message always: frozen attempts where there are any, else the preview of the next.
        if True:
            argv = argv + ["--message"]
            shows[f"{name}/{item['eventId']}/message"] = {
                "state": found["state"], "argv": argv, **python_show(found["state"], argv)}

# Previews the plain renderers produce: a revision request with many findings (overflow,
# restoration block, a note spanning lines, a heading-like id, no disposition) and a completion
# whose receipt claims criteria past the cap, both queued and never attempted.
case = td.Phases("test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt")
case.setUp()
completion, revision = case.correction_after_needs_changes()
findings = [{"id": f"c-{n}", "verdict": v, "note": note}
            for n, (v, note) in enumerate([("needs_changes", "line one\nline two"),
                                           ("unverified", None), ("verified", ""),
                                           (None, "undecided")] * 3)]
findings.insert(1, {"id": "FIX SCOPE: extra", "verdict": "needs_changes"})
findings.append({"id": "c-last", "verdict": "needs_changes", "restoration": {"note": "keep"}})
receipt = json.loads(case.store.one("SELECT receipt FROM events WHERE event_id = ?",
                                    (revision,))["receipt"])
receipt["criteria"] = findings
case.store.db.execute("UPDATE events SET receipt = ? WHERE event_id = ?",
                      (json.dumps(receipt), revision))
second = td.Phases("test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt")
second.setUp()
_relationship, queued = second.queued_event()
receipt = json.loads(second.store.one("SELECT receipt FROM events WHERE event_id = ?",
                                      (queued,))["receipt"])
receipt["criteria"] = [{"id": f"k-{n}", "verdict": "verified"} for n in range(12)]
receipt["manifestRef"] = "refs/manifest"
second.store.db.execute("UPDATE events SET receipt = ? WHERE event_id = ?",
                        (json.dumps(receipt), queued))
for name, owner, event in (("preview-revision", case, revision),
                           ("preview-completion", second, queued)):
    state = os.path.dirname(str(owner.store.path))
    argv = ["--event", event, "--message"]
    shows[name] = {"state": state, "argv": argv, **python_show(state, argv)}
    assert '"previewMessage"' in shows[name]["stdout"], shows[name]["stdout"]

any_state = results["test_a_queued_delivery_is_waiting_on_the_send_not_the_receipt"]["state"]
shows["unknown-event"] = {"state": any_state, "argv": ["--event", "no-such-event"],
                          **python_show(any_state, ["--event", "no-such-event"])}

json.dump({"status": results, "show": shows}, sys.stdout)
