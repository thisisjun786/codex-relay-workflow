"""Capture every real packet-check invocation made by each store-reception scenario.
The before-input DB and files are replayed independently by the built Go binary.
"""
import contextlib,importlib,io,json,os,sqlite3,sys,unittest
from pathlib import Path
root=Path(__file__).resolve().parents[4]
sys.path[:0]=[str(root/'packages/codex-session-relay/src'),str(root/'packages/codex-thread-bridge/src'),str(root/'packages/codex-session-relay')]
from codex_session_relay import cli,receiver
from codex_session_relay.errors import RelayError
module=importlib.import_module('tests.test_store_reception')
original=cli.main
captures=[]
keep=Path(sys.argv[1]);keep.mkdir(parents=True,exist_ok=True)

def capture(argv=None):
    argv=list(argv)
    state=Path(argv[argv.index('--state')+1])
    paths={}
    for option in ('--packet','--record','--observation','--ledger'):
        if option in argv:
            path=Path(argv[argv.index(option)+1]);paths[str(path)]=path.read_bytes().hex() if path.exists() and path.is_file() else None
    declared=state/'launch-policy.json'
    special={}
    if declared.is_symlink():special[str(declared)]={'symlink':os.readlink(declared)}
    elif declared.exists() and not declared.is_file():special[str(declared)]={'fifo':True}
    elif declared.is_file():
        paths[str(declared)]=declared.read_bytes().hex()
        try:
            named=json.loads(declared.read_text()).get('path')
            if named and Path(named).is_file():paths[named]=Path(named).read_bytes().hex()
        except (ValueError,RecursionError):pass
    else:paths[str(declared)]=None
    policy=os.environ.get('CODEX_THREAD_BRIDGE_EXECUTION_POLICY')
    if policy and Path(policy).is_file():paths[policy]=Path(policy).read_bytes().hex()
    database=state/'relay.sqlite3';backup=None
    if database.exists():
        backup=str(keep/f'db-{len(captures)}.sqlite3')
        source=sqlite3.connect(str(database));dest=sqlite3.connect(backup)
        try:source.backup(dest)
        finally:source.close();dest.close()
    output=io.StringIO();errors=io.StringIO()
    with contextlib.redirect_stdout(output),contextlib.redirect_stderr(errors):
        try: code=original(argv)
        except SystemExit as e:code=e.code
    printed=output.getvalue();sys.stdout.write(printed);sys.stderr.write(errors.getvalue())
    after={}
    if '--ledger' in argv:
        ledger=Path(argv[argv.index('--ledger')+1]);after[str(ledger)]=ledger.read_bytes().hex() if ledger.is_file() else None
    captures.append({'argv':argv,'policy':policy,'files':paths,'special':special,'database':backup,'stdout':printed,'stderr':errors.getvalue(),'exit':code,'after':after})
    return code
cli.main=capture
original_ladder=receiver.ladder
def captured_ladder(connection,**kwargs):
    # Direct daemon/library calls have no CLI; preserve the same entry point on Go's side.
    if not any(frame.function in ('capture','check') for frame in __import__('inspect').stack()[1:]):
        backup=str(keep/f'db-{len(captures)}.sqlite3')
        dest=sqlite3.connect(backup)
        try:connection.backup(dest)
        finally:dest.close()
        result=original_ladder(connection,**kwargs)
        captures.append({'library':'ladder','kwargs':kwargs,'database':backup,'stdout':json.dumps(result)})
        return result
    return original_ladder(connection,**kwargs)
receiver.ladder=captured_ladder
suite=unittest.TestSuite()
for name in json.loads(sys.argv[2]):
    classes=[c for c in module.__dict__.values() if isinstance(c,type) and issubclass(c,unittest.TestCase) and name in c.__dict__]
    if len(classes)!=1:raise RuntimeError((name,classes))
    suite.addTest(classes[0](name))
result=unittest.TestResult()
with contextlib.redirect_stdout(io.StringIO()),contextlib.redirect_stderr(io.StringIO()):suite.run(result)
if result.errors or result.failures:
    for test,error in result.errors+result.failures:print(str(test)+'\n'+error,file=sys.stderr)
    sys.exit(1)
print(json.dumps(captures))
