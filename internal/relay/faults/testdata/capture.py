"""Capture complete fault CLI replies and fault_* rows from the Python source.

argv: <state-dir> <observation-json>. Runs only under an isolated HOME/XDG/CODEX_HOME.
"""
import json
import pathlib
import sys

from codex_session_relay.clock import FakeClock
from codex_session_relay.faults import FaultLedger
from codex_session_relay.store import Store

state, raw = sys.argv[1:]
path = pathlib.Path(state) / "relay.sqlite3"
path.parent.mkdir(parents=True, exist_ok=True)
clock = FakeClock(start=100000)
db = Store(str(path))
try:
    ledger = FaultLedger(db, clock)
    requests = json.loads(raw)
    if not isinstance(requests, list):
        requests = [requests]
    answers = []
    for request in requests:
        if isinstance(request, dict) and "adoptionSeed" in request:
            seed = request["adoptionSeed"]
            with db.transaction() as connection:
                connection.execute("INSERT INTO fault_adoptions (fault_id, external_ref, scope, state, created_at, updated_at) VALUES (?,?,?,?,?,?)", (seed["faultId"], seed["externalRef"], json.dumps(seed["scope"], sort_keys=True), "pending", clock.iso(), clock.iso()))
            continue
        if isinstance(request, dict) and "policy" in request:
            ledger.set_policy(**request["policy"])
            continue
        if isinstance(request, dict) and "advanceSeconds" in request:
            clock.advance(request["advanceSeconds"])
            continue
        try:
            answers.append(ledger.record(request))
        except Exception as error:
            answers.append({"error": type(error).__name__, "reason": getattr(getattr(error, "reason", None), "value", None), "detail": str(error)})
    answer = answers[0] if len(answers) == 1 else answers
    rows = {}
    for (table,) in db.db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'fault_%' ORDER BY name"):
        found = [dict(row) for row in db.db.execute(f"SELECT * FROM {table} ORDER BY rowid")]
        if found:
            rows[table] = found
    print(json.dumps({"reply": answer, "tables": rows}, sort_keys=True))
finally:
    db.close()
