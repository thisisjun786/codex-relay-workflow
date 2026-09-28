"""D1: decoded lone-surrogate identities through both real adapter binaries."""
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading

binary, root, base = sys.argv[1], Path(sys.argv[2]), Path(sys.argv[3])
release = {'decision': 'release', 'state': 'unmanaged', 'hook_output': {}}


def run(label, session, item):
    home = base / label
    home.mkdir()
    relay = home / 'relay'
    relay.write_text('#!' + sys.executable + '\nimport sys\nsys.stdin.read()\nprint(' + repr(json.dumps(release)) + ')\n')
    relay.chmod(0o700)
    config = dict(configVersion=1, relayExecutable=str(relay), markerRoot=str(home / 'marker'),
                  dbPath=str(home / 'state/relay.sqlite3'), mode='observe', journalRoot=str(home / 'journal'))
    (home / 'crw-completion-hook.json').write_text(json.dumps(config))
    transcript = home / 'transcript.jsonl'
    rows = [{'type': 'event_msg', 'payload': {'type': 'task_started', 'turn_id': 'turn'}},
            {'type': 'event_msg', 'payload': {'type': 'item_completed', 'turn_id': 'turn', 'thread_id': session,
             'item': {'type': 'AgentMessage', 'id': item, 'content': [{'type': 'Text', 'text': 'DONE'}]}}}]
    transcript.write_text('\n'.join(map(json.dumps, rows)) + '\n')
    payload = dict(session_id=session, turn_id='turn', stop_hook_active=False,
                   last_assistant_message='DONE', transcript_path=str(transcript))
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'))
    env.pop('CODEX_SESSION_RELAY_STATE', None)
    listener, thread, failures = None, None, []
    if label.startswith('go'):
        (home / 'state').mkdir(mode=0o700)
        listener = socket.socket(socket.AF_UNIX)
        listener.bind(str(home / 'state/control.sock')); listener.listen(); listener.settimeout(5)
        def serve():
            try:
                with listener.accept()[0] as conn:
                    conn.settimeout(5)
                    with conn.makefile('rb') as stream:
                        request = json.loads(stream.readline())
                        assert request['params']['stopInput'] == payload
                        conn.sendall((json.dumps(release) + '\n').encode())
            except Exception as error:
                failures.append(repr(error))
        thread = threading.Thread(target=serve); thread.start()
    argv = [binary, 'hook'] if listener else [sys.executable, str(root / 'packages/codex-session-relay/src/codex_session_relay/stopadapter.py')]
    done = subprocess.run(argv, input=json.dumps(payload).encode(), capture_output=True, env=env, timeout=10)
    if thread:
        assert listener is not None
        thread.join(6); assert not thread.is_alive(); listener.close()
    assert not failures, failures
    assert (done.returncode, done.stdout, done.stderr) == (0, b'', b'')
    [path] = list((home / 'journal').glob('[0-9]*/*.json'))
    row = json.loads(path.read_text())
    row = {k: v for k, v in row.items() if k not in ('at', 'elapsedMs', 'guardElapsedMs', 'identityScanMs')}
    row['configuration'] = row['configuration'].replace(str(home), '<HOME>')
    row['eventIdentity']['transcriptPath'] = row['eventIdentity']['transcriptPath'].replace(str(home), '<HOME>')
    claims = sorted(p.name for p in (home / 'crw-completion-hook/stop-events').glob('*.json'))
    return {'row': row, 'claims': claims}

cases = [('s', 'item'), ('se\ud800ss', 'item'), ('se\udfffss', 'item'),
         ('s', '\ud800item'), ('s', 'mid\udc00dle'), ('s', 'end\udbff'),
         ('s\U0001f600\udc00', '\U0001d11e\ud800item')]
for index, (session, item) in enumerate(cases):
    py, go = run('python' + str(index), session, item), run('go' + str(index), session, item)
    assert py == go, (index, py, go)
print(json.dumps({'cases': len(cases), 'equal': True}))
