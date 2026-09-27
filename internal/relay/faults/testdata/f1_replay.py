"""Capture the CLI's complete streams, exit, and every SQLite table after a replay."""
import contextlib
import io
import json
import sqlite3
import sys
from codex_session_relay import cli, faults, faultsweep
from codex_session_relay.clock import FakeClock

cli.SystemClock = lambda: FakeClock(start=100000)
faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
faultsweep._now = lambda store: FakeClock(start=100000).iso()
state, *args = sys.argv[1:]
stdout, stderr = io.StringIO(), io.StringIO()
with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
    code = cli.main(['--state', state, '--json', *args])
with sqlite3.connect(state + '/relay.sqlite3') as db:
    db.row_factory = sqlite3.Row
    tables = {row[0]: [dict(item) for item in db.execute(f'SELECT * FROM {row[0]} ORDER BY rowid')]
              for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")}
print(json.dumps({'stdout': stdout.getvalue(), 'stderr': stderr.getvalue(), 'exit': code, 'tables': tables}))
