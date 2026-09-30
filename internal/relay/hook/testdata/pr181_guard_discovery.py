"""Compare actual guard CLI behavior when eager state selection cannot list roots.

usage: pr181_guard_discovery.py <binary> <repository root>
       pr181_guard_discovery.py - <repository root> prepare <home>
       pr181_guard_discovery.py - <repository root> python <home>

prepare lays the fixture out under home; python prints Python's CLI outcome for each case
there. The Go test records both (internal/testsupport/pyoracle) and runs the native side.
"""
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


def prepare(home):
    (home / 'blocked').write_text('not a directory')
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


CASES = [('unknown_override', lambda home: {'CODEX_SESSION_RELAY_STATE': '~crw_user_that_does_not_exist/state'}, 3),
         ('xdg_file', lambda home: {'XDG_STATE_HOME': str(home / 'blocked')}, 0),
         ('home_file', lambda home: {'HOME': str(home / 'blocked'), 'XDG_STATE_HOME': ''}, 0),
         ('unreadable_xdg', lambda home: {'XDG_STATE_HOME': str(home / 'locked')}, 3)]


def run(home, command, overrides):
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'), CODEX_SESSION_RELAY_STATE='')
    env.update(overrides)
    args = ['guard-evaluate', '--db-path', str(home / 'explicit.sqlite3'), '--marker-root', str(home / 'markers'), '--stop-input', str(home / 'stop.json'), '--no-record', '--now', '2026-01-01T00:00:00Z']
    return subprocess.run([*command, *args], env=env, capture_output=True, timeout=10)


if sys.argv[3:4] == ['prepare']:
    prepare(Path(sys.argv[4]))
    sys.exit()
if sys.argv[3:4] == ['python']:
    home = Path(sys.argv[4])
    print(json.dumps({name: dict(zip(('code', 'stdout', 'stderr'), (py.returncode, py.stdout.decode(), py.stderr.decode())))
                      for name, overrides, _ in CASES
                      for py in [run(home, [sys.executable, '-m', 'codex_session_relay.cli'], overrides(home))]}))
    sys.exit()

results = []
with tempfile.TemporaryDirectory(prefix='guard-discovery-') as temp:
    home = Path(temp)
    prepare(home)
    try:
        for name, overrides, expected in CASES:
            py = run(home, [sys.executable, '-m', 'codex_session_relay.cli'], overrides(home))
            go = run(home, [binary, 'relay'], overrides(home))
            assert (py.returncode, py.stdout, py.stderr) == (go.returncode, go.stdout, go.stderr), (name, py, go)
            assert go.returncode == expected
            if expected == 0:
                # The explicit store was really read: an empty readable store answers
                # relationship_absent, not state_unreadable or an unmanaged shortcut.
                assert json.loads(go.stdout)['receiptEvidence'] == 'relationship_absent'
            results.append({'case': name, 'exit': go.returncode, 'equal': True})
    finally:
        (home / 'locked').chmod(0o700)
print(json.dumps(results))
