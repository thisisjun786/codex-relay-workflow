# Construct identical isolated baseline stores for the two real CLI processes.
import contextlib, io, json, pathlib, sys
from codex_session_relay import cli
from codex_session_relay.store import Store
from codex_session_relay.registry import Registry

home = pathlib.Path(sys.argv[1])
state = home / 'state'
root = home / 'art'
root.mkdir(exist_ok=True)

def run(args):
    out, err = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = cli.main(['--state', str(state)] + args)
    if code:
        raise RuntimeError((code, out.getvalue(), err.getvalue()))
    return json.loads(out.getvalue())

r = run(['register', '--parent-task', 'P', '--parent-host', 'host', '--child-task', 'C', '--child-host', 'host', '--issue', 'I-1', '--artifact-root', str(root), '--allowed-recipient', 'P', '--dispatch-request-id', 'd', '--dispatch-turn-id', 'turn'])
run(['linkage-bind', '--role', 'parent', '--scope', 'PROJ', '--task', 'P', '--host', 'host'])
observation = {'schema':'fault-observation/1','product':'crw','faultClass':'report_omitted','severity':'broken','signature':{'relationship':'r','turn':'t'},'occurrenceKey':'a','scope':{'projectKey':'PROJ','issueKey':'I-1'},'detail':'omitted','evidence':[]}
f = run(['fault-observe', '--observation', json.dumps(observation)])
class Clock:
    def iso(self): return '2026-01-01T00:00:00+00:00'
s = Store(str(state/'relay.sqlite3'))
reg = Registry(s,Clock())
identity = dict(request_id='request', issue_key='I-2', request_fingerprint='fingerprint', fingerprint_version='v1', workspace=str(home), marker_root=str(home/'markers'), socket_identity='host', create_request_id='create', dispatch_request_id='dispatch')
reg.reserve_start(identity)
s.close()
print(json.dumps(dict(relationship=r['relationshipId'], fault=f['faultId'])))
