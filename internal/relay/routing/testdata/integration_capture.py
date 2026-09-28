"""Replay original product-routing scenarios; export boundary calls, not Go answers."""
import copy
import importlib.util
import inspect
import json
import pathlib
import sys
import unittest
from unittest import mock

root = pathlib.Path(__file__).resolve().parents[4]
package = root / 'packages/codex-session-relay'
sys.path[:0] = [str(package / 'src'), str(root / 'packages/codex-thread-bridge/src')]
spec = importlib.util.spec_from_file_location('routing_python_tests', package / 'tests/__init__.py', submodule_search_locations=[str(package / 'tests')])
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
from routing_python_tests import test_product_routing as scenarios
from routing_python_tests import test_product_routing_decisions as decisions
from codex_session_relay import faults, routing, products, digest, routes, intake, placement, completion, projects, ledger_port
from codex_session_relay.errors import RelayError

property_file = pathlib.Path(sys.argv[2])
property_id = sys.argv[1]
text = property_file.read_text()
if property_id.startswith('PRD-'):
    scenarios = decisions
    text = text.split('## test_product_routing_decisions.py')[1].split('## test_project_completion.py')[0]
else:
    text = text.split('## test_product_routing.py')[1].split('## test_product_routing_decisions.py')[0]
block = text.split(f'**{property_id}**')[1].split('\n- **')[0]
names = block.split('Tests: ')[1].split('\n')[0].split(', ')
if property_id == 'PRD-10':
    names = [n for n in names if n not in ('test_a_product_is_registered_bound_and_shown_from_the_command_line','test_a_binding_for_an_unregistered_product_is_a_refusal')]
if property_id == 'PRD-13':
    names = [n for n in names if n != 'test_the_route_commands_answer_the_refusal']
if property_id == 'PRD-17':
    names = ['test_classifying_into_a_product_that_does_not_watch_the_surface_is_refused']
records = []
depth = 0
case = None

def tables():
    names = [row[0] for row in case.store.db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='schema_meta' ORDER BY name")]
    return {name: [dict(row) for row in case.store.db.execute('SELECT * FROM "' + name + '" ORDER BY rowid')] for name in names}

def call(name, original):
    def wrapped(self, *args, **kwargs):
        global depth
        if depth:
            return original(self, *args, **kwargs)
        depth += 1
        record = {'operation': name, 'args': copy.deepcopy(args), 'kwargs': copy.deepcopy(kwargs), 'stamp': case.clock.iso(), 'now': case.clock.now()}
        record['missing'] = list(getattr(self, 'missing', getattr(getattr(self, 'port', None), 'missing', [])))
        if property_id == 'PR-22':
            record['transactionReads'] = []
            global active_record
            active_record = record
        if name == 'router.digest':
            record['pageSize'] = digest.PAGE
            if isinstance(routes.listing, mock.Mock):
                record['pageInjection'] = True
        if hasattr(self, 'port') and isinstance(self.port.queue, mock.Mock):
            record['failure'] = 'queue'
        if isinstance(intake._save, mock.Mock):
            record['failure'] = 'filing failed'
        try:
            answer = original(self, *args, **kwargs)
        except RelayError as error:
            record['expected'] = json.dumps({'error': 'refused', 'reason': error.reason.value, 'detail': str(error)}, sort_keys=True, ensure_ascii=False)
            record['tables'] = json.dumps(tables(), sort_keys=True, ensure_ascii=False)
            records.append(record)
            raise
        except RuntimeError as error:
            record['expected'] = json.dumps({'error': 'RuntimeError', 'detail': str(error)}, sort_keys=True, ensure_ascii=False)
            record['tables'] = json.dumps(tables(), sort_keys=True, ensure_ascii=False)
            records.append(record)
            raise
        else:
            record['expected'] = json.dumps(answer, sort_keys=True, ensure_ascii=False)
            record['wire'] = json.dumps(answer)
            record['tables'] = json.dumps(tables(), sort_keys=True, ensure_ascii=False)
            records.append(record)
            return answer
        finally:
            depth -= 1
    return wrapped

class DB:
    def __init__(self, connection): self.connection = connection
    def __getattr__(self, key): return getattr(self.connection, key)
    def execute(self, sql, args=()):
        if not depth and sql.lstrip().upper().startswith(('INSERT ', 'UPDATE ', 'DELETE ', 'DROP ', 'CREATE ')):
            records.append({'operation': 'sql', 'args': [sql, args], 'kwargs': {}, 'expected': 'null'})
        return self.connection.execute(sql, args)

router_methods = ('register_product','bind','set_policy','show_products','intake','classify','reconcile','evaluate_projects','check_completion','digest','show','registry','registries','bindings','policy','run_issue')
ledger_methods = ('record','get','canonical_id','set_target','adopt','move','request_update','queue','publication','publications','cancel','claim','operation','complete','fail','reconcile','record_fix','record_reverification','resolve','next','expire_leases','raise_notification','notifications','remediations','snapshot','set_policy','set_limit')
for name in router_methods:
    original = getattr(routing.ProductRouter, name)
    setattr(routing.ProductRouter, name, call('router.'+name, original))
for name in ledger_methods:
    original = getattr(faults.FaultLedger, name)
    setattr(faults.FaultLedger, name, call('ledger.'+name, original))
faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
if property_id == 'PRD-13':
    ledger_port.LedgerPort._require = call('port.require',ledger_port.LedgerPort._require)

active_record = None
if property_id == 'PR-22':
    for owner, method, label in ((placement,'decide','decide'),(completion,'evaluate','evaluate'),(projects,'_groups','groups'),(products,'read_binding','binding')):
        original = getattr(owner,method)
        def measured(*args,_original=original,_label=label,**kwargs):
            if active_record is not None:
                active_record['transactionReads'].append([_label,case.store.in_transaction])
            return _original(*args,**kwargs)
        setattr(owner,method,measured)

for name in names:
    classes = [c for c in vars(scenarios).values() if inspect.isclass(c) and issubclass(c, unittest.TestCase) and name in c.__dict__]
    if len(classes) != 1: raise RuntimeError((name, [c.__name__ for c in classes]))
    case = classes[0](name)
    records.append({'operation': 'reset', 'args': [], 'kwargs': {}, 'expected': 'null', 'scenario': name})
    # Instrument the store before ProductRoutingCase.setUp registers its snapshots.
    original_setup = scenarios.RelayTestCase.setUp
    def setup(self):
        original_setup(self)
        self.store.db = DB(self.store.db)
    with mock.patch.object(scenarios.RelayTestCase, 'setUp', setup):
        try:
            case.setUp()
            if property_id == 'PR-18':
                # Go's kind registry is static: replay this scenario's registered-holder
                # half on the same real ledger with the injected clock/token source.
                case.router.set_policy(scenarios.POLICY)
                scenarios.Projects.gamma(case,'cache','stale','g1')
                scenarios.Projects.gamma(case,'queue','lost','g2')
                row, = [r for r in case.ledger.next(limit=50) if r['kind']==projects.KIND]
                pid=row['publication_id']
                claim=case.ledger.claim(pid,owner='holder')
                operation=case.ledger.operation(pid,claim_token=claim['claimToken'])
                case.assertEqual('GMK',operation['payload']['team'])
            else:
                getattr(case, name)()
            if property_id == 'PR-25':
                records.append({'operation':'registry-installed','args':[],'kwargs':{},'expected':json.dumps({'project_create': projects.KIND in faults.KINDS,'completion_mismatch':products.MISMATCH in faults.CLASS_POLICY,'missing':ledger_port.missing()},sort_keys=True)})
        finally:
            case.doCleanups()
print(json.dumps(records, ensure_ascii=False))
