import contextlib,io,json,sqlite3,sys
from pathlib import Path
root=Path(__file__).resolve().parents[4]
sys.path[:0]=[str(root/'packages/codex-session-relay/src'),str(root/'packages/codex-thread-bridge/src')]
from codex_session_relay import cli,sync
from codex_session_relay.store import Store
class Clock:
 def now(self):return 1700000000.0
 def iso(self):return '2023-11-14T22:13:20.000000+00:00'
cli.SystemClock=Clock
CLAIM='claim-'+'x'*26
sync.secrets.token_hex=lambda n:CLAIM
base=Path(sys.argv[1]);state=base/'state';store=Store(state/'relay.sqlite3')
store.db.execute("INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','REL-1','active','parent','host','child','host',1,'[]','[]',?,?)",(Clock().iso(),Clock().iso()))
store.close()
commands=[['sync-target','--relationship','rel-1','--target-ref','doc-1'],['sync-progress','--relationship','rel-1'],['sync-next'],['sync-status']]
captures=[];identifier=None
for index in range(18):
 if index>=len(commands):break
 argv=commands[index]
 before=Store(state/'relay.sqlite3');backup=base/f'before-{index}.sqlite3';destination=sqlite3.connect(backup);before.db.backup(destination);destination.close();before.close()
 out=io.StringIO();err=io.StringIO()
 with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):code=cli.main(['--state',str(state)]+argv)
 result=json.loads(out.getvalue())
 if argv[0]=='sync-progress' and code==0:
  identifier=result['syncId'];commands.extend([['sync-operation','--sync',identifier],['sync-reconcile','--sync',identifier,'--observed','nothing'],['sync-claim','--sync',identifier,'--owner','main'],['sync-fail','--sync',identifier,'--claim-token',CLAIM,'--error','connector timed out'],['sync-retry','--sync',identifier],['sync-claim','--sync',identifier,'--owner','main']])
 if argv[0]=='sync-operation' and code==0:
  readback=base/'readback.txt';readback.write_text(result['block']);commands.extend([['sync-complete','--sync',identifier,'--claim-token',CLAIM,'--target-ref','doc-1','--readback','@'+str(readback)],['sync-status'],['sync-complete','--sync',identifier,'--claim-token','bad','--target-ref','doc-1','--readback','missing'],['sync-operation','--sync','unknown'],['sync-progress','--relationship','unknown']])
 after=Store(state/'relay.sqlite3');tables={r[0]:[dict(x) for x in after.all('SELECT * FROM '+r[0])] for r in after.all("SELECT name FROM sqlite_master WHERE type='table' AND name!='schema_meta' ORDER BY name")};after.close()
 captures.append({'argv':['--state',str(state)]+argv,'database':str(backup),'stdout':out.getvalue(),'stderr':err.getvalue(),'exit':code,'tables':tables})
print(json.dumps(captures))
