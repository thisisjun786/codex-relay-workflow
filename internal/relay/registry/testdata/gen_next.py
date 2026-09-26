"""Rows: [state, ack, deliveryState, holdReason, settingsHold, pacing, hostLost, supersession,
undeliveredReason, action]; needs_changes rows are corrections. Record assignment.completion_next_action / correction_next_action over a projection table.

  uv run --no-sync python internal/relay/registry/testdata/gen_next.py \
      | gzip -9n > internal/relay/registry/testdata/python_next.json.gz
"""
import itertools
import json
import sys

from codex_session_relay.assignment import completion_next_action, correction_next_action

states = ["queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "dispatched",
          "inbox_only", "acknowledged", "superseded"]
holds = [None, "attempt_cap", "push_channel_closed", "host_lost_turn", "unknown_send_lost",
         "unknown_send_undecided"]
settings_holds = [None,
                  {"kind": "withheld", "source": "attempt", "reason": "settings_not_preserved"},
                  {"kind": "withheld", "source": "pre_send", "reason": "setting_unobservable"},
                  {"kind": "withheld", "source": "undetermined", "reason": None},
                  {"kind": "capped", "source": "attempt", "reason": "settings_not_preserved"},
                  {"kind": "withheld", "source": "pre_send", "reason": "role_binding_mismatch"}]
pacings = [None, {"reason": "hourly_cap", "reopensAt": None}]
acks = [None, {"settlement": "verified", "accepted": True}, {"settlement": "unverified", "lastReason": None},
        {"settlement": "unverified", "lastReason": "revision_mismatch"}]
out = []
for state, hold, sh, pacing, lost in itertools.product(states, holds, settings_holds, pacings, (0, 1)):
    delivery = {"state": state, "holdReason": hold, "hostLostAttempts": lost, "pacing": pacing,
                "settingsHold": sh}
    for ack in acks:
        for assignment in ("received", "requested"):
            p = {"completion": {"eventId": "e", "ack": ack, "delivery": delivery}}
            out.append([assignment, ack, state, hold, sh, pacing, lost, None, None,
                        completion_next_action(assignment, p)])
    for supersession, reason in itertools.product((None, {"reason": "superseded_revision"}),
                                                  (None, {"source": "deliveries.hold_reason"})):
        p = {"correction": {"eventId": "e", "supersession": supersession,
                            "undeliveredReason": reason, "delivery": delivery}}
        out.append(["needs_changes", None, state, hold, sh, pacing, lost, supersession, reason,
                    correction_next_action("needs_changes", p)])
json.dump(out, sys.stdout, separators=(",", ":"))
sys.stdout.write("\n")
