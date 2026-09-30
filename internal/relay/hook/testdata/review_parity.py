"""Live checkout-Python/native boundary comparisons for review D1-D13."""
import contextlib
import errno
import hashlib
import importlib
import json
import os
from pathlib import Path
import selectors
import shutil
import socket
import subprocess
import sys
import threading
import time

from typing import Any

BINARY, root_arg, base_arg, GROUP = sys.argv[1:5]
ROOT, BASE = Path(root_arg), Path(base_arg)
# Python's side alone, recorded by the Go test (internal/testsupport/pyoracle): each pair's
# Python answer and snapshot, each Python CLI outcome, in order; the native side is not run.
PYTHON_ONLY = sys.argv[5:] == ['python']
RECORDED = []
ANSWER = sys.stdout
if PYTHON_ONLY:
    sys.stdout = sys.stderr
sys.path.insert(0, str(ROOT / 'scripts'))
completion = importlib.import_module('crw_runtime.completion')

RELEASE = dict(decision='release', state='unmanaged', hook_output={})


@contextlib.contextmanager
def peer(path, response=RELEASE):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    listener = socket.socket(socket.AF_UNIX)
    listener.bind(str(path)); listener.listen()
    read, write = os.pipe()
    failures, calls = [], []
    def serve():
        try:
            with selectors.DefaultSelector() as ready:
                ready.register(listener, selectors.EVENT_READ)
                ready.register(read, selectors.EVENT_READ)
                while True:
                    events = ready.select(15)
                    assert events, 'peer completion not signalled'
                    if any(key.fileobj == read for key, _ in events):
                        return
                    conn, _ = listener.accept()
                    with conn, conn.makefile('rb') as stream:
                        conn.settimeout(10)
                        raw = stream.readline()
                        if not raw:
                            continue
                        calls.append(json.loads(raw))
                        if response is None:
                            assert stream.read() == b''
                        else:
                            conn.sendall((json.dumps(response) + '\n').encode())
        except BaseException as error:
            failures.append(repr(error))
    worker = threading.Thread(target=serve); worker.start()
    try:
        yield calls
    finally:
        os.write(write, b'done'); worker.join(15)
        listener.close(); os.close(read); os.close(write); path.unlink(missing_ok=True)
        assert not worker.is_alive() and not failures, failures


def setup(name):
    home = BASE / name; home.mkdir(parents=True)
    relay = home / 'relay'
    relay.write_text('#!' + sys.executable + '\nimport sys\nsys.stdin.buffer.read()\nprint(' + repr(json.dumps(RELEASE)) + ')\n')
    relay.chmod(0o700)
    cfg: dict[str, Any] = dict(configVersion=1, mode='observe', relayExecutable=str(relay),
               markerRoot=str(home / 'markers'), dbPath=str(home / 'state/relay.sqlite3'),
               timeoutSeconds=5, journalRoot=str(home / 'journal'))
    (home / completion.CONFIG_NAME).write_text(json.dumps(cfg))
    transcript = home / 'transcript.jsonl'
    lines = [dict(type='event_msg', payload=dict(type='task_started', turn_id='t')),
             dict(type='event_msg', payload=dict(type='item_completed', turn_id='t', thread_id='s',
                  item=dict(type='AgentMessage', id='i', content=[dict(type='Text', text='DONE')])))]
    transcript.write_text('\n'.join(map(json.dumps, lines)) + '\n')
    payload: dict[str, Any] = dict(session_id='s', turn_id='t', stop_hook_active=False,
                   last_assistant_message='DONE', transcript_path=str(transcript))
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'),
               CODEX_SESSION_RELAY_STATE=str(home / 'state'), CRW_COMPLETION_HOOK_CONFIG='')
    return home, cfg, payload, env


def snapshot(home, directories=False):
    def norm(value):
        if isinstance(value, dict):
            return {k: norm(v) for k, v in value.items() if k not in ('at', 'claimedAt', 'elapsedMs', 'guardElapsedMs', 'identityScanMs', 'pid')}
        if isinstance(value, str):
            import re
            return re.sub(r'[0-9]{8}/[0-9a-f]{32}\.json', '<ROW>', value)
        return value
    records = {}
    for root in [home / 'journal', home / 'crw-completion-hook']:
        for p in root.rglob('*'):
            if p.is_file():
                value = json.loads(p.read_text())
                key = str(p.relative_to(home))
                if p.parent.name.isdigit():
                    key = 'row'
                records[key] = dict(value=norm(value), mode=p.stat().st_mode & 0o777)
            elif directories:
                records[str(p.relative_to(home))] = p.stat().st_mode & 0o777
        if directories and root.exists():
            records[str(root.relative_to(home))] = root.stat().st_mode & 0o777
    return json.dumps(records, sort_keys=True)


def clear(home):
    for p in [home / 'journal', home / 'crw-completion-hook']:
        shutil.rmtree(p, ignore_errors=False) if p.exists() else None


def invoke(label, home, payload, env, args=(), mask=0o022, extra=None):
    command = [BINARY, 'hook'] if label == 'go' else [sys.executable, str(ROOT / 'scripts/completion_hook.py')]
    if extra is not None and label == 'python':
        command = [sys.executable, '-c', extra, str(ROOT / 'scripts'), *args]
        args = ()
    if isinstance(payload, Path):
        # Size parity is not an input-arrival deadline test: the complete bytes
        # and EOF exist before either process starts its own budget clock.
        with payload.open('rb') as stdin:
            done = subprocess.run([*command, *args], stdin=stdin, cwd=home, env=env, capture_output=True, timeout=15, umask=mask)
    else:
        raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        done = subprocess.run([*command, *args], input=raw, cwd=home, env=env, capture_output=True, timeout=15, umask=mask)
    assert done.returncode == 0 and done.stderr == b'', done
    return done.stdout


def pair(home, cfg, payload, env, args=(), mask=0o022, directories=False, response=RELEASE):
    pyout = invoke('python', home, payload, env, args, mask)
    expected = snapshot(home, directories)
    clear(home)
    if PYTHON_ONLY:
        RECORDED.append(dict(kind='pair', stdout=pyout.decode(), snapshot=json.loads(expected)))
        return json.loads(expected)
    with peer(Path(cfg['dbPath']).parent / 'control.sock', response):
        goout = invoke('go', home, payload, env, args, mask)
    actual = snapshot(home, directories)
    assert (pyout, expected) == (goout, actual), (pyout, goout, expected, actual)
    return json.loads(actual)


def paths(group):
    cases = ['env', 'argv_over_env', 'empty_env', 'empty_arg'] if group == 'D1' else ['relative_arg', 'relative_home', 'lexical', 'symlink']
    for name in cases:
        home, cfg, payload, env = setup(name)
        alt = home / 'alt'; alt.mkdir(); path = alt / 'cfg.json'; path.write_text(json.dumps(cfg))
        args = []
        if name in ('env', 'argv_over_env', 'empty_arg'):
            env['CRW_COMPLETION_HOOK_CONFIG'] = str(path)
            (home / completion.CONFIG_NAME).unlink()
        if name == 'argv_over_env':
            env['CRW_COMPLETION_HOOK_CONFIG'] = str(home / 'missing')
            args = [str(path)]
        if name == 'empty_arg': args = ['']
        if name == 'relative_arg': args = ['alt/cfg.json']
        if name == 'relative_home': env['CODEX_HOME'] = '.'
        if name == 'lexical': args = [str(home) + '/alt/../alt/./cfg.json']
        if name == 'symlink':
            (home / 'link.json').symlink_to(path)
            args = ['link.json']
        result = pair(home, cfg, payload, env, args)
        expected = str(completion.configuration_path(home, env, args[0] if args else None))
        if name == 'relative_arg': expected = str(path)
        if name == 'symlink': expected = str(home / 'link.json')
        assert result['row']['value']['configuration'] == expected, result
        if group == 'D2' and not PYTHON_ONLY:
            clear(home)
            invoke('go', home, payload, env, args)  # no socket: decision-22 reader path
            [p] = (home / 'journal').glob('*/*.json')
            assert completion._native_prescan_unreachable(json.loads(p.read_text()))
            verdict = completion.stop_events([home / 'journal'])
            assert verdict['verdict'] == 'TRUE', verdict
        print(name, 'equal')


def constants():
    for name, value in [('nan', float('nan')), ('positive', float('inf')), ('negative', -float('inf'))]:
        home, cfg, payload, env = setup(name)
        cfg['extra'] = [value, 'NaN', {'Infinity': value}]
        (home / completion.CONFIG_NAME).write_text(json.dumps(cfg))
        transcript = Path(payload['transcript_path'])
        transcript.write_text(transcript.read_text().replace('"payload": {', '"extra": ' + json.dumps(value) + ', "payload": {'))
        payload['extra'] = value
        result = pair(home, cfg, payload, env)
        assert result['row']['value']['eventIdentity']['established']
        for field in ['session_id', 'turn_id', 'stop_hook_active']:
            clear(home); altered = dict(payload); altered[field] = value
            result = pair(home, cfg, altered, env)
            assert result['row']['value']['adapterOutcome'] == 'guard_answered'
        # Same loader on guard stdin; compare full CLI bytes, including echoed constants.
        args = ['guard-evaluate', '--marker-root', str(home / 'markers'), '--now', '2026-01-01T00:00:00Z', '--no-record']
        raw = json.dumps(dict(session_id=value, turn_id=value, extra=[value])).encode()
        py = subprocess.run([sys.executable, '-m', 'codex_session_relay.cli', *args], input=raw, env=env, capture_output=True, timeout=10)
        if PYTHON_ONLY:
            RECORDED.append(dict(kind='cli', code=py.returncode, stdout=py.stdout.decode(), stderr=py.stderr.decode()))
            continue
        go = subprocess.run([BINARY, 'relay', *args], input=raw, env=env, capture_output=True, timeout=10)
        assert (py.returncode, py.stdout, py.stderr) == (go.returncode, go.stdout, go.stderr), (py, go)
        print(name, 'all loaders equal')


def budgets():
    for budget in [7, 7.01, 8, 8.99, 9]:
        home, cfg, payload, env = setup(str(budget))
        cfg.update(owner='plugin', adapterEntryPoint=str(home / 'adapter'), adapterInterpreter=sys.executable, timeoutSeconds=budget)
        (home / completion.CONFIG_NAME).write_text(json.dumps(cfg))
        result = pair(home, cfg, payload, env)
        assert bool(result) == (budget == 7), (budget, result)
        print(budget, 'equal')


def dial_errors():
    for number in [errno.EINVAL, errno.ELOOP]:
        home, cfg, payload, env = setup(str(number))
        state = home / ('x' * 130) if number == errno.EINVAL else home / 'state'
        state.mkdir(); sock = state / 'control.sock'
        if number == errno.ELOOP: sock.symlink_to('control.sock')
        cfg['dbPath'] = str(state / 'relay.sqlite3')
        (home / completion.CONFIG_NAME).write_text(json.dumps(cfg))
        # Python's equivalent boundary is Popen raising this errno after identity
        # and claims; it has no native socket. Keep the concrete failing path equal.
        source = 'import sys,runpy;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;from unittest.mock import patch;import errno,os\nsys.argv=["completion_hook.py"]\nwith patch.object(completion.subprocess,"Popen",side_effect=OSError(' + str(number) + ',os.strerror(' + str(number) + '),' + repr(str(sock)) + ')):\n runpy.run_module("completion_hook",run_name="__main__")'
        invoke('python', home, payload, env, extra=source)
        expected = snapshot(home); clear(home)
        if PYTHON_ONLY:
            RECORDED.append(dict(kind='snapshot', snapshot=json.loads(expected)))
            continue
        invoke('go', home, payload, env)
        actual = snapshot(home); assert expected == actual, (expected, actual)
        out = subprocess.run([sys.executable, str(ROOT / 'scripts/stop_events.py'), '--journal-root', str(home / 'journal')], capture_output=True, timeout=10)
        answer = json.loads(out.stdout)
        assert answer['rowsUnreadable'] == [], answer
        print(errno.errorcode[number], 'row equal; reader', answer['verdict'])


def utf8():
    home, cfg, payload, env = setup('utf8')
    for raw in [b'{"a":"\xff"}', b'{"a":"\xe2\x82"}', b'\x80']:
        args = ['guard-evaluate', '--marker-root', cfg['markerRoot']]
        # Strict UTF-8 stdin is the Python relay's supported environment.
        env['PYTHONIOENCODING'] = 'utf-8:strict'
        py = subprocess.run([sys.executable, '-m', 'codex_session_relay.cli', *args], input=raw, env=env, capture_output=True, timeout=10)
        assert py.returncode == 3
        if PYTHON_ONLY:
            RECORDED.append(dict(kind='cli', code=py.returncode, stdout=py.stdout.decode(), stderr=py.stderr.decode()))
            continue
        go = subprocess.run([BINARY, 'relay', *args], input=raw, env=env, capture_output=True, timeout=10)
        assert (py.returncode, py.stdout, py.stderr) == (go.returncode, go.stdout, go.stderr), (py, go)
    # Settings environment must never affect guard-evaluate's independent inputs.
    env['CRW_COMPLETION_HOOK_CONFIG'] = str(home / 'missing')
    for command in [[sys.executable, '-m', 'codex_session_relay.cli']] + ([] if PYTHON_ONLY else [[BINARY, 'relay']]):
        out = subprocess.run([*command, *args, '--now', '2026-01-01T00:00:00Z'], input=b'{}', env=env, capture_output=True, timeout=10)
        assert out.returncode == 0 and json.loads(out.stdout)['state'] == 'unmanaged'
    print('stdin host errors equal; guard ignores hook settings')


if GROUP in ('D1', 'D2'): paths(GROUP)
elif GROUP == 'D3': constants()
elif GROUP == 'D4': budgets()
elif GROUP == 'D5': dial_errors()
elif GROUP == 'D7':
    for mask in [0o002, 0o022]:
        home, cfg, payload, env = setup(str(mask)); pair(home, cfg, payload, env, mask=mask, directories=True)
        print(oct(mask), 'directory and file modes equal')
elif GROUP == 'D9':
    home, cfg, payload, env = setup('large');payload['padding'] = 'x' * (5 << 20)
    stdin = home / 'prefilled-stdin.json'
    stdin.write_bytes(json.dumps(payload).encode())
    pair(home, cfg, stdin, env);print('5 MiB prefilled stdin payload equal')
elif GROUP == 'D10': utf8()
elif GROUP == 'D13':
    home, cfg, payload, env = setup('timeout')
    cfg['timeoutSeconds'] = 2
    (home / completion.CONFIG_NAME).write_text(json.dumps(cfg))
    with peer(home / 'state/control.sock', None):
        assert invoke('go', home, payload, env) == b''
    [path] = (home / 'journal').glob('[0-9]*/*.json')
    row = json.loads(path.read_text())
    assert row['adapterOutcome'] == 'guard_timed_out' and row['processEnding'] == 'timed_out'
    answer = completion.stop_events([home / 'journal'])
    assert answer['rowsUnreadable'] == [], answer
    original = answer
    print('native timeout row:', json.dumps(row))
    row['detail'] = 'a different diagnostic, not a reader token'
    path.write_bytes(completion._record_bytes(row))
    assert completion.stop_events([home / 'journal']) == original
    print('timeout reader result is unchanged after replacing only detail')
else: raise AssertionError(GROUP)
if PYTHON_ONLY:
    ANSWER.write(json.dumps(RECORDED))
