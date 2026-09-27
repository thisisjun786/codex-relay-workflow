"""Direct FC-9 / FC-26 oracle: complete replies, observer calls and fault tables."""
import contextlib
import json
from pathlib import Path
import sys
from unittest import mock
from codex_session_relay import faults, faultsweep, omitted
from codex_session_relay.clock import FakeClock
from codex_session_relay.store import Store, resolve_state_dir

path, action, variant = sys.argv[1:]
clock = FakeClock(start=100000)
faultsweep._now = lambda store: clock.iso()
store = Store(path)
try:
    ledger = faults.FaultLedger(store, clock)
    calls = []
    replies = []
    if action == 'managed':
        real = variant.startswith('real_')
        if real:
            request = store.one('SELECT * FROM managed_start_requests')
            from codex_session_relay import marker
            directory = marker.assignment_dir(request['marker_root'], request['workspace'], marker.assignment_id(request['dispatch_request_id']))
            intent_path = directory / 'intent.json'
            if intent_path.exists():
                intent = json.loads(intent_path.read_text())
                intent['dbPath'] = str(store.path)
                intent_path.write_text(json.dumps(intent))
        def observe(selection, **kw):
            calls.append({'selection': selection, **kw})
            if variant == 'error' and kw['turn'] == 'turn-9':
                raise OSError('marker unreadable')
            if variant == 'none':
                return None
            answer = {'schema': 'reporting-observation/1', 'reportingState': 'unreported',
                      'selectors': {'turn': kw['turn'], 'session': kw['session']}}
            if variant != 'unnamed':
                answer['relationshipId'] = 'rel'
            return answer
        faultsweep.INSTALLATION = {'package': 'codex-session-relay', 'version': 'test', 'location': 'test'}
        with contextlib.nullcontext() if real else mock.patch.object(omitted, 'observe', observe):
            for _ in range(2):
                batch = faultsweep.sweep(store, selection=resolve_state_dir(str(Path(path).parent)) if real else 'the-selection', now=clock.iso())
                replies.append(faultsweep.record_all(ledger, batch, store=store))
    elif action == 'pre_issue':
        def check(context):
            calls.append({'publication': context['publication'], 'fault': context['fault'], 'now': context['now'],
                          'count': context['db'].execute('SELECT COUNT(*) FROM fault_publications WHERE publication_id=?', (context['publication']['publication_id'],)).fetchone()[0]})
            if variant == 'write':
                context['db'].execute("UPDATE fault_ledger SET detail='written'")
                return None
            if variant == 'cancel':
                return {'cancel': 'a suitable project was bound after the claim'}
            if variant == 'hold':
                return {'hold': 'still measuring', 'seconds': 17}
            if variant == 'invalid':
                return 'unexpected'
            return None
        faults.register_kind('project_create_check', creates=True, requires_issue=False,
                             target='team', evidence='block', confirm=lambda expected, observed: [], pre_issue=check)
        identifier = store.one('SELECT fault_id FROM fault_ledger')['fault_id']
        queued = ledger.queue(identifier, kind='project_create_check', trigger='need', payload={'name': 'Ops'})
        replies.append(queued)
        pub = queued['publicationId']
        faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
        claim = ledger.claim(pub, owner='writer-A')
        replies.append(claim)
        try:
            operation = ledger.operation(pub, claim_token=claim['claimToken'])
            replies.append(operation)
            if variant == 'accept':
                replies.append(ledger.complete(pub, claim_token=claim['claimToken'], readback=operation['block'], external_ref='PROJECT-7'))
        except faults.FaultRefused as error:
            assert error.reason is not None
            replies.append({'error': 'FaultRefused', 'reason': error.reason.value, 'detail': error.detail})
    elif action == 'prune_alias':
        replies.append(ledger.prune('alias', keep=1)['removed'])
    elif action == 'sweep_no_project':
        reading = {'schema': 'reporting-observation/1', 'relationshipId': 'rel',
                   'selectors': {'turn': 'turn'}, 'reportingState': 'unreported'}
        batch = faultsweep.sweep(store, readings=[reading])
        replies.append(faultsweep.record_all(ledger, batch, store=store))
    elif action == 'workspace':
        batch = faultsweep.sweep(store, scope={'workspace': 'unassigned'})
        replies.append(faultsweep.record_all(ledger, batch, store=store))
    elif action == 'claim_budget':
        pub = store.one('SELECT publication_id FROM fault_publications')['publication_id']
        faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
        try:
            replies.append(ledger.claim(pub, owner='writer'))
        except faults.FaultRefused as error:
            assert error.reason is not None
            replies.append({'error': 'FaultRefused', 'reason': error.reason.value, 'detail': error.detail})
    elif action == 'fields_create':
        # Replay test_a_create_confirmed_by_fields_is_refused, then compare the
        # built-in declaration too: invalid static manifests must not evade FC-27.
        try:
            faults.register_kind('fields_create_test', creates=True, requires_issue=True,
                                 target=None, evidence='fields', confirm=lambda expected, observed: [])
            replies.append(None)
        except ValueError as error:
            replies.append({'error': 'ValueError', 'detail': str(error)})
        replies.append({'registered': 'fields_create_test' in faults.KINDS})
        spec = faults.KINDS['update_record']
        replies.append({key: spec[key] for key in ('creates', 'requires_issue', 'target', 'evidence')})
    else:
        raise ValueError(action)
    tables = {row[0]: [dict(item) for item in store.all(f'SELECT * FROM {row[0]} ORDER BY rowid')]
              for row in store.all("SELECT name FROM sqlite_master WHERE type='table' AND (name LIKE 'fault_%' OR name='journal' OR name LIKE 'supervisor_%') ORDER BY name")}
    print(json.dumps({'replies': replies, 'calls': calls, 'tables': tables}))
finally:
    store.close()
