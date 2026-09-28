"""Exercise the actual sync-outbox test scenarios, including their verdict caller."""
import contextlib,copy,importlib,io,json,sqlite3,sys,unittest
from pathlib import Path
root=Path(__file__).resolve().parents[4]
sys.path[:0]=[str(root/'packages/codex-session-relay/src'),str(root/'packages/codex-thread-bridge/src'),str(root/'packages/codex-session-relay')]
from codex_session_relay import ack,sync
from codex_session_relay.errors import RelayError
module=importlib.import_module('tests.test_sync_outbox')
keep=Path(sys.argv[1]);keep.mkdir(parents=True,exist_ok=True)
calls=[];depth=0;counter=0

def token(_):
    global counter
    counter+=1
    return f'{counter:032x}'
sync.secrets.token_hex=token

def tables(store):
    names=[r[0] for r in store.all("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name") if r[0]!='schema_meta']
    return {name:[dict(r) for r in store.all('SELECT * FROM '+name)] for name in names}

def wrap(cls,name):
    original=getattr(cls,name)
    def run(self,*args,**kwargs):
        global depth
        outer=depth==0
        if outer:
            before=tables(self.store)
            path=str(keep/f'{len(calls)}.sqlite3')
            dest=sqlite3.connect(path)
            try:self.store.db.backup(dest)
            finally:dest.close()
            record={'method':name,'args':copy.deepcopy(args),'kwargs':copy.deepcopy(kwargs),'database':path,'now':self.clock.now(),'counter':counter,'explode':name=='record_verdict' and getattr(getattr(self,'sync',None),'enqueue_verdict_in',None).__name__=='explode'}
        depth+=1
        try:
            answer=original(self,*args,**kwargs)
            if outer:record['reply']=dict(answer) if isinstance(answer,sqlite3.Row) else copy.deepcopy(answer)
            return answer
        except RelayError as e:
            if outer:record['error']={'reason':e.reason.value,'detail':e.detail}
            raise
        except RuntimeError as e:
            if outer:record['runtimeError']=str(e)
            raise
        finally:
            depth-=1
            if outer:
                after=tables(self.store)
                record['tables']={k:v for k,v in after.items() if v!=before[k]}
                record['before']=before
                calls.append(record)
    setattr(cls,name,run)
wrap(ack.AckService,'record_verdict')
for name in ('set_target','claim','operation','reconcile','complete','fail','retry','snapshot','next'):
    wrap(sync.SyncOutbox,name)
suite=unittest.TestSuite()
for name in json.loads(sys.argv[2]):
    found=[c for c in module.__dict__.values() if isinstance(c,type) and issubclass(c,unittest.TestCase) and name in c.__dict__]
    if len(found)!=1:raise RuntimeError((name,found))
    suite.addTest(found[0](name))
result=unittest.TestResult()
with contextlib.redirect_stdout(io.StringIO()):suite.run(result)
if result.errors or result.failures:
    for test,error in result.errors+result.failures:print(str(test)+'\n'+error,file=sys.stderr)
    sys.exit(1)
print(json.dumps(calls))
