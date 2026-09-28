"""Compare native hook and live Python adapter through their process entry points."""
import contextlib
import json
import os
import pathlib
import re
import socket
import subprocess
import sys
import threading
from typing import Any

binary, tree, source = sys.argv[1:]
tree, source = pathlib.Path(tree), pathlib.Path(source)
adapter = source / 'packages/codex-session-relay/src/codex_session_relay/stopadapter.py'
fixture = json.loads((source / 'packages/codex-session-relay/tests/fixtures/stop_event_r1.json').read_text())
release = dict(decision='release', state='unmanaged', hook_output={})
hold = dict(decision='block', state='receipt_missing', observation='receipt_missing',
            assignmentId='a' * 64, counters={'holdsThisTurn': 0}, recordedAs='hook/s/t/0',
            hook_output=dict(decision='block', reason='verify the child', **{'continue': True}))


def normalize(value: Any, home: pathlib.Path, path=()) -> Any:
    if isinstance(value, str):
        if path in {('configuration',), ('eventIdentity', 'transcriptPath'),
                    ('claimedBy', 'journalRoot'), ('claimedBy', 'hostLedger')}:
            return value.replace(str(home), '<HOME>')
        if path in {('attemptRow',), ('claimedBy', 'attemptRow')}:
            assert re.fullmatch(r'[0-9]{8}/[0-9a-f]{32}\.json', value), value
            return '<ROW>'
        return value
    if isinstance(value, list):
        return [normalize(v, home, path + (i,)) for i, v in enumerate(value)]
    if isinstance(value, dict):
        volatile = {('claimedBy', 'pid'), ('at',), ('claimedAt',), ('elapsedMs',),
                    ('guardElapsedMs',), ('identityScanMs',)}
        return {k: normalize(v, home, path + (k,)) for k, v in value.items()
                if path + (k,) not in volatile}
    return value


def run(label, name, response):
    home = tree / name / label
    home.mkdir(parents=True)
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'),
               CODEX_SESSION_RELAY_STATE=str(home / 'state'))
    relay = home / 'relay'
    relay.write_text('#!' + sys.executable + '\nimport sys,json,signal\n'
                     'sys.stdin.buffer.read()\n' +
                     ('signal.pause()\n' if name == 'timeout' else '') +
                     'sys.stdout.write(' + repr(json.dumps(response)) + ')\n')
    relay.chmod(0o700)
    settings = dict(configVersion=1, relayExecutable=str(relay), markerRoot=str(home / 'marker'),
                    dbPath=str(home / 'state/relay.sqlite3'), mode='observe', timeoutSeconds=0.4 if name == 'timeout' else 5,
                    journalRoot=str(home / 'journal'), journalPolicy='every_invocation')
    config = home / 'crw-completion-hook.json'
    if name != 'missing':
        config.write_text('{' if name == 'unreadable' else json.dumps(settings))
    transcript = home / 'transcript.jsonl'
    stop = fixture['stops'][0]
    transcript.write_text('\n'.join(x.replace('<CODEX_HOME>', str(home)) for x in fixture['transcriptLines'][:stop['linesAtStop']]) + '\n')
    payload = dict(stop['payload'], transcript_path=str(transcript), cwd=str(home))
    raw = b'not json' if name == 'malformed' else json.dumps(payload).encode()
    listener, thread, failure = None, None, []
    invocations = 2 if name == 'duplicate' else 1
    if label == 'go':
        (home / 'state').mkdir(mode=0o700)
        listener = socket.socket(socket.AF_UNIX)
        listener.bind(str(home / 'state/control.sock'))
        listener.listen()
        listener.settimeout(10)
        def serve():
            try:
                for _ in range(invocations):
                    with contextlib.closing(listener.accept()[0]) as conn:
                        conn.settimeout(10)
                        with conn.makefile('rb') as stream:
                            line = stream.readline()
                            if not line:
                                continue  # duplicate exits after the dial, before a guard request
                            request = json.loads(line)
                            assert request['method'] == 'guard-evaluate'
                            assert request['params']['stopInput'] == payload
                            if name == 'timeout':
                                assert stream.read() == b''  # exact disconnect signal, no timing sleep
                            else:
                                conn.sendall((json.dumps(response) + '\n').encode())
            except Exception as error:
                failure.append(repr(error))
        if name not in ('missing', 'unreadable', 'malformed'):
            thread = threading.Thread(target=serve)
            thread.start()
    command = [binary, 'hook'] if label == 'go' else [sys.executable, str(adapter)]
    outputs = []
    for _ in range(invocations):
        done = subprocess.run(command, input=raw, capture_output=True, env=env, timeout=10)
        outputs.append(dict(exit=done.returncode, stdout=done.stdout.decode(), stderr=done.stderr.decode()))
    if thread:
        thread.join(timeout=10)
        assert not thread.is_alive(), 'control peer did not finish'
    if listener:
        listener.close()
    assert not failure, failure
    rows = [normalize(json.loads(p.read_text()), home) for p in sorted((home / 'journal').glob('[0-9]*/*.json'))]
    rows.sort(key=lambda r: str(r.get('acceptance')))
    files = {}
    for root in [home / 'journal/accepted', home / 'crw-completion-hook/stop-events']:
        for p in sorted(root.glob('*.json')):
            files[str(p.relative_to(home))] = normalize(json.loads(p.read_text()), home)
            assert p.stat().st_mode & 0o777 == 0o600
    return dict(outputs=outputs, rows=rows, files=files)

results = []
for name, response in [('hold', hold), ('release', release), ('duplicate', release),
                       ('malformed', release), ('missing', release), ('unreadable', release), ('timeout', release)]:
    py, go = run('python', name, response), run('go', name, response)
    if name == 'timeout':
        # Decision 24: native cancellation has no process group. Diagnostic prose
        # is intentionally different; all machine-consumed outcome fields agree.
        py['rows'][0].pop('detail')
        go['rows'][0].pop('detail')
    assert py == go, (name, py, go)
    results.append(dict(scenario=name, python=py, go=go, equal=True))
print(json.dumps(results))
