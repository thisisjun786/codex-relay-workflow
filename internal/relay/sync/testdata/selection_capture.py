import contextlib,io,json,os,sys
from pathlib import Path
root=Path(__file__).resolve().parents[4]
sys.path[:0]=[str(root/'packages/codex-session-relay/src'),str(root/'packages/codex-thread-bridge/src'),str(root/'packages/codex-session-relay')]
from codex_session_relay import cli,store
from tests.test_reception_findings import packet_kwargs,P2C
from codex_session_relay import packets
base=Path(sys.argv[1]);state=base/'state';socket=base/'contested.sock'
for name in ('aaaa444444444444','bbbb444444444444'):
    store.Store(state/'codex-session-relay'/name/'relay.sqlite3',socket_path=str(socket)).close()
packet=base/'packet.json';packet.write_text(json.dumps(packets.compose(**packet_kwargs(P2C,'assignment'))))
record=base/'record.json';record.write_text('{}')
os.environ.pop('CODEX_SESSION_RELAY_STATE',None)
os.environ['XDG_STATE_HOME']=str(state)
sys.argv[0]='crw relay'
results=[]
for extra in (['--record',str(record)],['--receiver','01child-task','--ledger',str(base/'ledger.json'),'--applied'],['--receiver','01child-task']):
 argv=['--socket',str(socket),'packet-check','--packet',str(packet)]+extra
 out=io.StringIO();err=io.StringIO()
 with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):code=cli.main(argv)
 results.append({'argv':argv,'stdout':out.getvalue(),'stderr':err.getvalue(),'exit':code})
print(json.dumps(results))
