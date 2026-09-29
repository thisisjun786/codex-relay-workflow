"""Fixtures for a routed Stop's selection refusals and the owner's own write locations.

prepare <home> <override|ambiguous>: the hook's environment sees one store for its socket, the
owner's sees two; expected.json is the CLI's local answer under the owner's configuration.
paths <home>: an owner's own marker root and store beside another root and store.
owner <state>: a live Python control.sock owner in this process's environment that prints
"ready", serves until a line arrives on stdin, then prints how many requests it answered.
"""
import contextlib
import json
import os
from pathlib import Path
import subprocess
import sys

from codex_session_relay import marker
from codex_session_relay.store import Store

NOW = '2026-01-01T00:00:00.123456+00:00'


def ready_for_review(root: Path, work: Path) -> dict:
    work.mkdir(exist_ok=True)
    directory = marker.assignment_dir(root, work, marker.assignment_id('dispatch'))
    for name, record in {
        'intent.json': {'dispatchRequestIdHash': marker.assignment_id('dispatch')},
        'bound.json': {'sessionId': 's'},
        'relationship.json': {'relationshipId': 'r'},
        'claims/s/claim.json': {'sessionId': 's', 'dispatchRequestId': 'dispatch'},
        'dispositions/s/t.json': {'sessionId': 's', 'turnId': 't', 'outcome': 'ready_for_review'},
    }.items():
        marker.publish(directory / name, record, root=root)
    return dict(cwd=str(work), session_id='s', turn_id='t', stop_hook_active=False)


def prepare(home: Path, case: str) -> None:
    wanted, state, root = home / 'app.sock', home / 'state', home / 'markers'
    base = {key: value for key, value in os.environ.items() if key != 'CODEX_SESSION_RELAY_STATE'}
    base.update(HOME=str(home), CODEX_HOME=str(home))
    owner = dict(base, XDG_STATE_HOME=str(home / 'xdg'), CODEX_SESSION_RELAY_MARKER_ROOT=str(root))
    if case == 'override':
        owner['CODEX_SESSION_RELAY_STATE'] = str(state)
    hook = dict(base, XDG_STATE_HOME=str(home / 'hook-xdg'), CODEX_SESSION_RELAY_STATE=str(state))
    for name in ('a', 'b'):
        with contextlib.closing(Store(str(home / 'xdg/codex-session-relay' / name / 'relay.sqlite3'),
                                      socket_path=str(wanted))):
            pass
    with contextlib.closing(Store(str(state / 'relay.sqlite3'), socket_path=str(wanted))):
        pass
    stop = ready_for_review(root, home / 'work')
    program = str(Path(sys.executable).parent / 'codex-session-relay')
    argv = [program, '--socket', str(wanted), 'guard-evaluate', '--marker-root', str(root),
            '--mode', 'hold', '--now', NOW]
    local = argv if case == 'override' else [program, '--state', str(state), *argv[1:]]
    expected = subprocess.run(local, input=json.dumps(stop).encode(), env=owner, cwd=home,
                              capture_output=True, timeout=30)
    assert expected.returncode == 2 and not expected.stderr, expected
    assert json.loads(expected.stdout)['reason'] == 'ambiguous_state_directory', expected
    (home / 'fixture.json').write_text(json.dumps(dict(
        ownerEnv=owner, hookEnv=hook, state=str(state), socket=str(wanted), root=str(root),
        stop=stop, program=program, now=NOW, argv=argv, expected=json.loads(expected.stdout))))


def paths(home: Path) -> None:
    own, elsewhere, work = home / 'markers', home / 'elsewhere', home / 'work'
    stop = ready_for_review(own, work)
    ready_for_review(elsewhere, work)
    for name in ('state', 'other'):
        with contextlib.closing(Store(str(home / name / 'relay.sqlite3'))):
            pass
    (home / 'link').symlink_to(home / 'state', target_is_directory=True)
    (home / 'fixture.json').write_text(json.dumps(dict(stop=stop, now=NOW)))


def owner(state: str) -> None:
    from codex_session_relay import control

    answer = control.GuardServer._answer
    asked = []

    def counted(self, connection):
        asked.append(True)
        return answer(self, connection)

    control.GuardServer._answer = counted
    server = control.GuardServer(state)
    print('ready', flush=True)
    sys.stdin.readline()
    server.close()
    print(json.dumps(dict(asked=len(asked))), flush=True)


if __name__ == '__main__':
    action = sys.argv[1]
    if action == 'prepare':
        prepare(Path(sys.argv[2]), sys.argv[3])
    elif action == 'paths':
        paths(Path(sys.argv[2]))
    else:
        owner(sys.argv[2])
