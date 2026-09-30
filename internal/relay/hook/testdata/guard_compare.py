"""Real marker/store/receipt fixtures replayed through Python and the built Go CLI."""
import contextlib
import json
import os
import pathlib
import subprocess
import sys

from codex_session_relay import guard, intent, manifest, marker
from codex_session_relay.clock import FakeClock
from codex_session_relay.models import Endpoint, TurnRef
from codex_session_relay.registry import Registry
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.store import Store

binary, home, selected = sys.argv[1:4]
home = pathlib.Path(home)
# prepare <case>: lay the case's fixture out under <home>/<case> and print Python's answer (the
# envelope, the observation record it wrote and removed, the mode), recorded by the Go test
# (internal/testsupport/pyoracle), which runs the native CLI itself.
PREPARE = selected == 'prepare'
os.environ.update(HOME=str(home), CODEX_HOME=str(home), XDG_STATE_HOME=str(home / 'xdg'))
now = '2026-01-01T00:06:00+00:00'
cases = ['unmanaged', 'unclaimed', 'uncorrelated', 'unbound', 'other', 'unregistered',
         'undeclared', 'ready_missing', 'ready_receipted', 'ready_staged', 'artifacts_changed',
         'frozen', 'generation', 'dispatch', 'generation_absent', 'inactive', 'foreign_turn',
         'malformed_disposition', 'unreadable_disposition', 'malformed_marker', 'bad_identity',
         'active', 'hold_spent', 'generation_spent', 'window_spent', 'observe',
         'in_progress', 'blocked_needs_input', 'interrupted', 'failed', 'no_record',
         'frozen_at_depth', 'frozen_past_depth']
# json.loads's C scanner spends one level of its recursion budget per container, and the budget
# left where the guard reads a frozen MANIFEST.json is 9998: the outer object and 9997 nested
# lists are read, and one list more is a RecursionError deliverable_state does not catch.
depths = {'frozen_at_depth': 9997, 'frozen_past_depth': 9998}
if PREPARE:
    cases = [sys.argv[4]]
elif selected != 'all':
    cases = selected.split(',')


def own_by_go(db):
    """Hand the fixture's store to Go, as test_fence's stamp does. The Go CLI evaluates in-process
    only a store it may evaluate: a Stop that reads another runtime's store is routed to that
    owner's control.sock, and refused when none answers (cutover.md, Go finding owner=python)."""
    import sqlite3
    from codex_session_relay import inbox, ownership
    connection = sqlite3.connect(db)
    with connection:
        connection.execute("UPDATE schema_meta SET value='go' WHERE key='owner'")
    connection.close()
    path = pathlib.Path(db)
    record = json.loads((path.parent / 'takeover.json').read_bytes())
    record.update(owner='go', database=ownership.physical(path))
    (path.parent / 'takeover.json').write_bytes(inbox.canonical(record))


answers = []
for name in cases:
    base = home / name
    work, root, db = base / 'work', base / 'markers', base / 'state' / 'relay.sqlite3'
    work.mkdir(parents=True)
    stop = dict(cwd=str(work), session_id='child', turn_id='turn', stop_hook_active=False)
    clock = FakeClock()
    with contextlib.closing(Store(str(db))) as store:
        registry = Registry(store, clock)
        relation = registry.register(parent=Endpoint('parent', 'host'), child=Endpoint('child', 'host', cwd=str(work)),
                                     issue_key='T-1', artifact_roots=[str(work)], allowed_recipients=['parent'],
                                     dispatch_request_id='dispatch', dispatch_turn_id='turn')
        rid = relation['relationshipId']
        assignment = marker.assignment_id('dispatch')
        directory = marker.assignment_dir(root, work, assignment)
        if name != 'unmanaged':
            intent.declare_intent(root, workspace=work, dispatch_request_id='dispatch', issue_key='T-1',
                                  declared_at=now, db_path=str(db))
            if name != 'unclaimed':
                intent.publish_claim(root, workspace=work, assignment=assignment, session_id='child',
                                     dispatch_request_id='dispatch', first_turn_id='turn', at=now)
            if name != 'unbound':
                intent.bind(root, workspace=work, assignment=assignment, session_id='child', task_id='child', at=now)
            if name != 'unregistered':
                intent.register_relationship(root, workspace=work, assignment=assignment, relationship_id=rid,
                                             dispatch_request_id='dispatch', db_path=str(db), at=now)
        if name == 'uncorrelated':
            p = directory / 'claims/child/claim.json'
            value = json.loads(p.read_text()); value['dispatchRequestId'] = 'foreign'; p.write_text(json.dumps(value))
        if name == 'other':
            stop['session_id'] = 'other'
        if name in ['ready_missing', 'ready_receipted', 'ready_staged', 'artifacts_changed', 'frozen',
                    'generation', 'dispatch', 'generation_absent', 'inactive', 'foreign_turn', *depths]:
            intent.publish_disposition(root, workspace=work, assignment=assignment, session_id='child',
                                       turn_id='turn', outcome='ready_for_review', at=now)
        if name in ['ready_receipted', 'ready_staged', 'artifacts_changed', 'frozen', 'foreign_turn', *depths]:
            artifact = work / 'out.txt'; artifact.write_text('work')
            entries, _ = manifest.build([str(artifact)], [str(work)])
            revision = manifest.revision_hash(entries)
            from codex_session_relay.identity import event_id
            ref = TurnRef('child', 'turn', 'inProgress' if name == 'ready_staged' else 'completed')
            payload = dict(eventId=event_id(rid, 1, revision, 'ready_for_review', turn_id='turn', attempt=1),
                           relationshipId=rid, executionGeneration=1, attempt=1, revisionHash=revision,
                           outcome='ready_for_review', producer='child', turnRef=ref.to_record(),
                           manifest=[e.to_record() for e in entries], emittedAt=clock.iso())
            if name == 'frozen' or name in depths:
                payload['manifestRef'] = manifest.freeze(entries, base / 'frozen')
            ReceiptIntake(store, registry, clock).accept_child_receipt(payload, observation=ref)
            if name in ['artifacts_changed', 'frozen', *depths]:
                artifact.write_text('changed')
            if name in depths:
                document = base / 'frozen' / 'MANIFEST.json'
                frozen = json.loads(document.read_text())
                document.write_text(json.dumps(frozen)[:-1] + ', "x": ' + '[' * depths[name] + ']' * depths[name] + '}')
            if name == 'foreign_turn':
                store.db.execute("UPDATE events SET turn_id='foreign'")
        if name == 'generation':
            store.db.execute('UPDATE relationships SET execution_generation=2')
        if name == 'dispatch':
            store.db.execute("UPDATE generations SET dispatch_request_id='foreign'")
        if name == 'generation_absent':
            store.db.execute('DELETE FROM generations')
        if name == 'inactive':
            store.db.execute("UPDATE relationships SET status='stopped'")
        if name in ['in_progress', 'blocked_needs_input', 'interrupted', 'failed']:
            intent.publish_disposition(root, workspace=work, assignment=assignment, session_id='child', turn_id='turn', outcome=name, at=now)
        if name in ['malformed_disposition', 'unreadable_disposition']:
            p = directory / 'dispositions/child/turn.json'; p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text('[]' if name == 'malformed_disposition' else '{')
        if name == 'malformed_marker':
            (directory / 'bound.json').write_text('[]')
        if name == 'bad_identity':
            stop['turn_id'] = '../escape'
        if name == 'active':
            stop['stop_hook_active'] = True
        if name in ['hold_spent', 'generation_spent', 'window_spent']:
            count = dict(hold_spent=1, generation_spent=2, window_spent=3)[name]
            for n in range(count):
                folder = directory if name != 'window_spent' else directory.parent / ('f' * 63 + str(n))
                guard.reserve_hold(folder, session_id='child', turn_id='turn' if name == 'hold_spent' else str(n), at=now, mode='hold', root=root)
    own_by_go(db)
    import sqlite3
    with contextlib.closing(sqlite3.connect(db)) as conn:
        before = list(conn.iterdump())
    mode = 'observe' if name in ['observe', 'no_record'] else 'hold'
    stop_path = base / 'stop.json'; stop_path.write_text(json.dumps(stop))
    expected = guard.evaluate(root, stop, now=now, mode=mode, record=name != 'no_record')
    # Restore only files produced by this evaluation; pre-existing hold history remains.
    record = expected.get('recordedAs')
    expected_record = None
    if record:
        p = directory / (record + '.json'); expected_record = p.read_bytes(); p.unlink()
    if expected['decision'] == 'block':
        (directory / 'hook/child/turn/hold.json').unlink()
    if PREPARE:
        print(json.dumps(dict(stdout=json.dumps(expected, indent=2) + '\n', mode=mode, noRecord=name == 'no_record',
                              record=str((directory / (record + '.json')).relative_to(base)) if record else None,
                              recordText=expected_record.decode() if expected_record is not None else None)))
        sys.exit()
    argv = [binary, 'relay', '--state', str(base / 'state'), 'guard-evaluate', '--marker-root', str(root),
            '--stop-input', str(stop_path), '--mode', mode, '--now', now]
    if name == 'no_record':
        argv.append('--no-record')
    done = subprocess.run(argv, capture_output=True, check=False, timeout=15)
    actual = json.loads(done.stdout)
    assert done.returncode == 0 and not done.stderr, (name, done.returncode, done.stderr)
    assert actual == expected, (name, expected, actual)
    # FULL byte envelope, including key order, as consumed by cached adapters.
    assert done.stdout == (json.dumps(expected, indent=2) + '\n').encode(), (name, done.stdout, expected)
    if expected_record is not None:
        assert (directory / (record + '.json')).read_bytes() == expected_record, name
    import sqlite3
    with contextlib.closing(sqlite3.connect(db)) as conn:
        assert list(conn.iterdump()) == before, (name, 'guard changed relay evidence')
    assert not (home / 'crw-completion-hook/stop-events').exists(), 'legacy command re-claimed event'
    answers.append(name)
print(json.dumps(answers))
