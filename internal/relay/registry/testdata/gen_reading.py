"""Record delivery.settings_hold_reading over a table of SETTINGS_HOLD_COLUMNS rows, each with
the recovery status would attach (settings_hold_recovery of the reading's kind/reason/source).

  uv run --no-sync python internal/relay/registry/testdata/gen_reading.py \
      | gzip -9n > internal/relay/registry/testdata/python_reading.json.gz
Rows: [columns, reading, recovery-or-null].
"""
import itertools
import json
import sys

from codex_session_relay.delivery import settings_hold_reading
from codex_session_relay.settings import settings_hold_recovery


def packed(seq, detail):
    return json.dumps({"seq": seq, "detail": json.dumps(detail) if detail is not None else None})


sent = [None,
        packed(10, {"requestId": "r1", "settingsRefusal": {"reason": "settings_not_preserved", "field": "runtimeWorkspaceRoots"}}),
        packed(10, {"requestId": "r1", "settingsRefusal": None}),
        packed(10, {"requestId": "r1", "settingsRefusal": {"reason": 7}}),
        packed(10, {"requestId": "r1"}),
        "not json"]
reconciled = [None, packed(30, {"settingsRefusal": {"reason": "setting_unobservable", "field": 3}}),
              packed(5, {"settingsRefusal": None})]
presend = [None,
           packed(20, {"operation": "settings_check", "reason": "settings_unavailable", "detail": "no row"}),
           packed(20, {"operation": "lifecycle_read", "reason": "recipient_archived"}),
           packed(20, {"operation": "role_check", "reason": "role_binding_mismatch", "detail": 9}),
           packed(40, {"operation": 5, "reason": "relationship_not_active"})]
states = [("withheld_pre_send", None), ("withheld_pre_send", "attempt_cap"),
          ("withheld_pre_send", "host_lost_turn"), ("inbox_only", None), ("queued", None)]
times = [(None, None, None), ("2026-01-01T00:00:01", None, None),
         ("2026-01-01T00:00:01", "2026-01-01T00:00:02", None),
         ("2026-01-01T00:00:01", None, "2026-01-01T00:00:02"),
         ("2026-01-01T00:00:01", "2026-01-01T00:00:01", None)]
out = []
for (state, hold), s, r, p, request, (settings_at, lifecycle_at, inactive_at) in itertools.product(
        states, sent, reconciled, presend, (None, "r1"), times):
    row = {"sh_state": state, "sh_hold_reason": hold, "sh_settled_sent": s,
           "sh_settled_reconciled": r, "sh_presend": p, "sh_request": request,
           "sh_settings_at": settings_at, "sh_lifecycle_at": lifecycle_at,
           "sh_inactive_at": inactive_at}
    reading = settings_hold_reading(row)
    recovery = None
    if reading["hold"] is not None:
        recovery = settings_hold_recovery(reading["kind"], reading["hold"]["reason"],
                                          reading["hold"]["source"])
    out.append([row, reading, recovery])
json.dump(out, sys.stdout, separators=(",", ":"))
sys.stdout.write("\n")
