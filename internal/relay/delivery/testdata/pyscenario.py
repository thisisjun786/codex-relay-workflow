"""Drives the real Python delivery package over a fixture tree Go shares, then dumps the store.

argv: <tree> <scenario>. The tree is the Go test's t.TempDir(): Python's RelayTestCase is pointed
at it, so artifact paths, revision hashes and event ids are the ones the Go run derives.
"""
import json
import os
import sqlite3
import sys
import tempfile

TREE, NAME = sys.argv[1], sys.argv[2]
tempfile.mkdtemp = lambda prefix=None: TREE

from tests.support import CHILD, PARENT, DeliveryTestCase  # noqa: E402
from codex_session_relay.errors import RelayError  # noqa: E402


class Case(DeliveryTestCase):
    def runTest(self):
        pass


def refusal(call, *args, **kwargs):
    try:
        return {"ok": call(*args, **kwargs)}
    except RelayError as error:
        return {"reason": error.reason.value if error.reason else None, "detail": error.detail}


def plain(value):
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, dict):
        return {k: plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    try:
        return {k: plain(value[k]) for k in value.keys()}
    except Exception:  # noqa: BLE001
        return repr(value)


c = Case()
c.setUp()
out = {}
exec(open(os.path.join(os.path.dirname(__file__), "scenarios", NAME + ".py")).read())
c.store.db.commit() if c.store.db.in_transaction else None
db = sqlite3.connect(os.path.join(TREE, "state", "relay.sqlite3"))
db.row_factory = sqlite3.Row
tables = {}
for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
    rows = [dict(r) for r in db.execute(f"SELECT * FROM {name} ORDER BY rowid")]
    if rows:
        tables[name] = rows
print(json.dumps({"out": plain(out), "tables": tables, "sends": [list(s) for s in c.adapter.sends]}, default=repr))
