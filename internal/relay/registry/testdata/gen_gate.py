"""Record delivery.authorized_settings (the pre-send settings gate) for rows written raw.

  uv run --no-sync python internal/relay/registry/testdata/gen_gate.py \
      > internal/relay/registry/testdata/python_gate.json
Each case writes its row (or none) under task "01parent-task", unbound, and records the gate's
answer: {"settings": data, "settingsFree": bool} or {"refused": {reason, detail}}.
"""
import json
import os
import sys
import tempfile

from codex_session_relay import rolepolicy
from codex_session_relay.delivery import authorized_settings
from codex_session_relay.errors import RelayError
from codex_session_relay.store import Store

cases = json.load(open("internal/relay/registry/testdata/gate_cases.json"))
rolepolicy.reset()
out = {}
for name, row in cases.items():
    directory = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
    store = Store(os.path.join(directory, "relay.sqlite3"))
    if row is not None:
        store.db.execute("INSERT INTO authorized_settings (task_id, settings, source, recorded_at)"
                         " VALUES (?,?,?,?)", ("01parent-task", json.dumps(row), "raw", "t"))
    try:
        settings = authorized_settings(store, "01parent-task")
        out[name] = {"settings": settings.data, "settingsFree": settings.settings_free_resume,
                     "resumeParams": settings.resume_params("t-1")}
    except RelayError as error:
        out[name] = {"refused": {"reason": error.reason.value, "detail": error.detail}}
    store.close()
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
