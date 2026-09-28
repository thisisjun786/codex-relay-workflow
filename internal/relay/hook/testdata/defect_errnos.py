"""D3: real nonblocking socket failures, Python errno spelling and row readers."""
import errno
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
from unittest import mock

from codex_session_relay import stopadapter

binary, root = sys.argv[1], Path(sys.argv[2])
sys.path.insert(0, str(root / 'scripts'))
from crw_runtime import completion

results = []
for kind in ['EAGAIN', 'ENOTDIR']:
    with tempfile.TemporaryDirectory(prefix='d3-') as temp:
        home = Path(temp); state = home / 'state'; endpoint = state / 'control.sock'
        config = dict(configVersion=1, relayExecutable=str(home / 'absent'), markerRoot=str(home / 'marker'),
                      dbPath=str(state / 'relay.sqlite3'), mode='observe', journalRoot=str(home / 'journal'))
        settings = home / 'crw-completion-hook.json'; settings.write_text(json.dumps(config))
        held = []
        if kind == 'ENOTDIR':
            state.write_text('not a directory')
        else:
            state.mkdir(); listener = socket.socket(socket.AF_UNIX); held.append(listener)
            listener.bind(str(endpoint)); listener.listen(0)
            # Fill until the kernel, not a sleep, reports the backlog bound.
            for _ in range(128):
                client = socket.socket(socket.AF_UNIX); held.append(client); client.setblocking(False)
                try:
                    client.connect(str(endpoint))
                except BlockingIOError:
                    break
            else:
                raise AssertionError('backlog did not fill')
        probe = socket.socket(socket.AF_UNIX); probe.setblocking(False)
        try:
            probe.connect(str(endpoint))
            raise AssertionError('expected dial failure')
        except OSError as failure:
            number = failure.errno
        finally:
            probe.close()
        assert errno.errorcode[number] == kind
        env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'))
        env.pop('CODEX_SESSION_RELAY_STATE', None)
        done = subprocess.run([binary, 'hook'], input=b'{"session_id":"s"}', capture_output=True, env=env, timeout=5)
        assert (done.returncode, done.stdout, done.stderr) == (0, b'', b'')
        [path] = list((home / 'journal').glob('[0-9]*/*.json')); go = json.loads(path.read_text())
        # Python's writer observes the same OSError via its process boundary.
        # Its identity scan differs by design; compare the failure observation itself.
        with mock.patch.object(stopadapter.subprocess, 'Popen', side_effect=OSError(number, os.strerror(number), str(endpoint))):
            python = stopadapter.invoke_guard(config, b'{}')
        assert go['errno'] == python['errno'] == kind, (go, python)
        assert go['detail'] == python['detail']
        assert completion._native_prescan_unreachable(go) and completion._row_shape(go)
        reading = completion.stop_events([home / 'journal'])
        assert reading['verdict'] == 'TRUE' and not reading['rowsUnreadable'], reading
        results.append({'errno': number, 'python': python['errno'], 'go': go['errno'], 'reader': reading['verdict'], 'row': go})
        for resource in held:
            resource.close()
print(json.dumps({'errnos': results}))
