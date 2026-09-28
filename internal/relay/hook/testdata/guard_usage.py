"""Legacy guard CLI usage envelopes through the built multicall binary."""
import json
import os
import pathlib
import subprocess
import sys

binary, home = sys.argv[1:]
home = pathlib.Path(home)
os.environ.update(HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'))
(home / 'bad-utf8').write_bytes(b'\xff')
(home / 'object').write_text('{}')
(home / 'list').write_text('[]')
(home / 'bad-json').write_text('{')
cases = [(['--stop-input', str(home / 'absent')], b''),
         (['--stop-input', str(home / 'bad-utf8')], b''),
         (['--stop-input', str(home / 'list')], b''),
         (['--stop-input', str(home / 'bad-json')], b''),
         (['--mode', 'hold', '--no-record', '--stop-input', str(home / 'object')], b''),
         ([], b'not json'), ([], b'[]')]
for args, payload in cases:
    go = subprocess.run([binary, 'relay', 'guard-evaluate', *args], input=payload, capture_output=True, timeout=10)
    py = subprocess.run([sys.executable, '-m', 'codex_session_relay.cli', 'guard-evaluate', *args], input=payload, capture_output=True, timeout=10)
    assert (go.returncode, go.stdout, go.stderr) == (py.returncode, py.stdout, py.stderr), (args, go, py)
print(json.dumps({'equal': len(cases)}))
