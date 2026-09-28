"""The deadline-bearing native hook must classify a valid large marker like Python."""
import contextlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading

from late_verdict import setup
from codex_session_relay import guard

binary, home = sys.argv[1], Path(sys.argv[2])
directory = setup(home)
# Prove the live bounded reader, not only the unbounded legacy command.
intent = directory / 'intent.json'
value = json.loads(intent.read_text()); value['padding'] = 'x' * (5 << 20)
intent.write_text(json.dumps(value))
payload = (home / 'stop.json').read_bytes()
config = json.loads((home / 'crw-completion-hook.json').read_text())
expected = guard.evaluate(config['markerRoot'], json.loads(payload), mode='hold',
                          now='2026-01-01T00:00:00Z', db_path=config['dbPath'])
assert expected['decision'] == 'block' and expected['state'] == 'receipt_missing', expected
files = {p.name: json.loads(p.read_text()) for p in (directory/'hook/s/t').glob('*.json')}
for p in (directory/'hook/s/t').glob('*.json'): p.unlink()
import sqlite3
with contextlib.closing(sqlite3.connect(config['dbPath'])) as db:
    db.execute("INSERT OR REPLACE INTO schema_meta VALUES('owner','go')"); db.commit()
(home/'state/takeover.json').write_text('{"owner":"go","phase":"active"}')
listener = socket.socket(socket.AF_UNIX); listener.bind(str(home/'state/control.sock')); listener.listen(); listener.settimeout(10)
failures = []
def serve():
    try:
        conn, _ = listener.accept()
        with conn:
            conn.settimeout(10)
            assert conn.recv(1) == b'', 'owned hook unexpectedly used RPC'
    except BaseException as error:
        failures.append(repr(error))
worker = threading.Thread(target=serve); worker.start()
env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), CODEX_SESSION_RELAY_STATE=str(home/'state'), CRW_COMPLETION_HOOK_CONFIG='')
try:
    done = subprocess.run([binary,'hook'],input=payload,env=env,capture_output=True,timeout=10)
finally:
    worker.join(12); listener.close()
assert not worker.is_alive() and not failures, failures
assert (done.returncode,done.stdout,done.stderr)==(0,json.dumps(expected['hook_output']).encode(),b''),done
[rowpath] = (home/'journal').glob('*/*.json');row=json.loads(rowpath.read_text())
assert row['adapterOutcome']=='guard_answered' and row['guardState']=='receipt_missing' and row['held'] is True,row
actual = {p.name:json.loads(p.read_text()) for p in (directory/'hook/s/t').glob('*.json')}
for entries in (files,actual):
    for record in entries.values():record.pop('at')  # independent real clocks
assert files==actual,(files,actual)
print(json.dumps(dict(equal=True,stdout=done.stdout.decode(),guardState=row['guardState'],marker_bytes=intent.stat().st_size)))
