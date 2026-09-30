"""Compare actual guard CLI behavior when eager state selection cannot list roots."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from contextlib import closing

import sqlite3

from codex_session_relay.store import Store
from codex_session_relay import marker, ownership

binary, root = sys.argv[1], Path(sys.argv[2])
results = []
with tempfile.TemporaryDirectory(prefix='guard-discovery-') as temp:
    home = Path(temp); (home / 'blocked').write_text('not a directory')
    workspace = home / 'work'; workspace.mkdir()
    assignment = marker.assignment_id('dispatch')
    directory = marker.assignment_dir(home / 'markers', workspace, assignment)
    for relative, value in {
        'intent.json': {'dispatchRequestIdHash': assignment},
        'bound.json': {'sessionId': 's'},
        'relationship.json': {'relationshipId': 'r'},
        'claims/s/claim.json': {'sessionId': 's', 'dispatchRequestId': 'dispatch'},
        'dispositions/s/t.json': {'sessionId': 's', 'turnId': 't', 'outcome': 'ready_for_review'},
    }.items():
        marker.publish(directory / relative, value, root=home / 'markers')
    (home / 'stop.json').write_text(json.dumps({'cwd': str(workspace), 'session_id': 's', 'turn_id': 't'}))
    with closing(Store(str(home / 'explicit.sqlite3'))):
        pass
    # A store neither runtime fences, which both CLIs evaluate in-process: a fenced one is read
    # only by its owner, and the other runtime's CLI routes the Stop to that owner's control.sock
    # (cutover.md, Go finding owner=python).
    with closing(sqlite3.connect(home / 'explicit.sqlite3')) as db, db:
        db.executemany('DELETE FROM schema_meta WHERE key=?', [(key,) for key in ownership.KEYS])
    (home / 'takeover.json').unlink()
    (home / 'write-gate.lock').unlink()
    locked = home / 'locked'; locked.mkdir(); locked.chmod(0)
    try:
        cases = [('unknown_override', {'CODEX_SESSION_RELAY_STATE': '~crw_user_that_does_not_exist/state'}, 3),
                 ('xdg_file', {'XDG_STATE_HOME': str(home / 'blocked')}, 0),
                 ('home_file', {'HOME': str(home / 'blocked'), 'XDG_STATE_HOME': ''}, 0),
                 ('unreadable_xdg', {'XDG_STATE_HOME': str(locked)}, 3)]
        for name, overrides, expected in cases:
            env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'), CODEX_SESSION_RELAY_STATE='')
            env.update(overrides)
            args = ['guard-evaluate', '--db-path', str(home / 'explicit.sqlite3'), '--marker-root', str(home / 'markers'), '--stop-input', str(home / 'stop.json'), '--no-record', '--now', '2026-01-01T00:00:00Z']
            py = subprocess.run([sys.executable, '-m', 'codex_session_relay.cli', *args], env=env, capture_output=True, timeout=10)
            go = subprocess.run([binary, 'relay', *args], env=env, capture_output=True, timeout=10)
            assert (py.returncode, py.stdout, py.stderr) == (go.returncode, go.stdout, go.stderr), (name, py, go)
            assert go.returncode == expected
            if expected == 0:
                # The explicit store was really read: an empty readable store answers
                # relationship_absent, not state_unreadable or an unmanaged shortcut.
                assert json.loads(go.stdout)['receiptEvidence'] == 'relationship_absent'
            results.append({'case': name, 'exit': go.returncode, 'equal': True})
    finally:
        locked.chmod(0o700)
print(json.dumps(results))
