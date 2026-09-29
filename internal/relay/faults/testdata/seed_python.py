"""Seed the Python-owned comparison store through the retained Python fence's own writer.

A runtime writes only a store it owns, so the statements a test seeds into both stores reach
Python's through codex_session_relay.store.Store, each in autocommit as Go's store runs them.
The statements arrive on stdin as [{"sql": ..., "args": [...]}, ...].
"""
import json
import sys

from codex_session_relay.store import Store

statements = json.load(sys.stdin)
store = Store(sys.argv[1])
try:
    for statement in statements:
        store.db.execute(statement['sql'], statement.get('args') or [])
finally:
    store.close()
