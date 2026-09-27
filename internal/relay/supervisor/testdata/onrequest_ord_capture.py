"""Capture the two todo-21 OnRequestSupervisor handoff scenarios whole.

The real Python fake host and supervisor channel run unchanged. Output includes every public
reply used by the tests and every populated SQLite row, including JSON columns as stored bytes.
"""
import json
import os
import sqlite3
import sys
import tempfile

from codex_session_relay import supervisorchannel
from tests.test_on_request_delivery import OnRequestSupervisor
from tests.test_supervisor_channel import SUPERVISOR

MODE, ROOT = sys.argv[1:3]
TREE = os.path.join(ROOT, "tree")
os.makedirs(TREE, exist_ok=True)
tempfile.mkdtemp = lambda *args, **kwargs: TREE
supervisorchannel.secrets.token_hex = lambda n: "00" * n
case = OnRequestSupervisor("test_an_on_request_supervisor_receives_the_push")
case.setUp()
try:
    case.adapter.threads[SUPERVISOR].approval_policy = "on-request"
    result = {}
    if MODE == "push":
        _obligation, message_id = case.staged()
        setup_source = sqlite3.connect(case.store.path)
        setup_target = sqlite3.connect(os.path.join(ROOT, "staged.sqlite3"))
        setup_source.backup(setup_target)
        setup_target.close()
        setup_source.close()
        result["attempt"] = case.channel.attempt(message_id, case.adapter)
        result["row"] = dict(case.channel.get(message_id))
    else:
        existing = case.adapter.start_turn(SUPERVISOR, status="inProgress")
        case.clock.advance(600)
        _obligation, message_id = case.staged()
        setup_source = sqlite3.connect(case.store.path)
        setup_target = sqlite3.connect(os.path.join(ROOT, "staged.sqlite3"))
        setup_source.backup(setup_target)
        setup_target.close()
        setup_source.close()
        case.adapter.script("steer_existing")
        result["attempt"] = case.channel.attempt(message_id, case.adapter)
        result["readback"] = case.read_back(message_id, existing.turn_id)
        result["row"] = dict(case.channel.get(message_id))
        before = len(case.adapter.sends)
        case.clock.advance(100000)
        result["second"] = case.channel.attempt(message_id, case.adapter, now=case.clock.now())
        result["resent"] = len(case.adapter.sends) - before
    result["messageId"] = message_id
    tables = {}
    db = sqlite3.connect(case.store.path)
    db.row_factory = sqlite3.Row
    for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
        rows = [dict(row) for row in db.execute(f"SELECT * FROM {name} ORDER BY rowid")]
        if rows:
            tables[name] = rows
    db.close()
    result["tables"] = tables
    print(json.dumps(result))
finally:
    case.doCleanups()
