import json,sys
from codex_session_relay import report
from codex_session_relay.errors import ReceiptRefused
HEAD="a1b2c3d4e5f60718293a4b5c6d7e8f9012345678";BASE="c56576d5be412b5bc352dd93b9eb37ab279a12f6"
def clean():return {"hasNextPage":False,"pagesRead":1,"totalCount":1,"threadsSeen":["PRRT_ready"],"unresolved":0}
def handoff():return {"isDraft":False,"baseVerifiedAt":"2026-09-20T09:00:00Z","requiredDeclared":["dev-gate"],"checks":[{"runId":"run-dev-gate","name":"dev-gate","headSha":HEAD,"conclusion":"success","attempt":1}],"reviewCoverage":clean(),"threadDispositions":[{"threadId":"PRRT_ready","disposition":"fixed","evidence":"addressed and rechecked on this head","addressedBy":"a1b2c3d"}],"criterionEvidence":[],"limitations":[]}
def outcome(fn):
 try:return {"ok":fn()}
 except ReceiptRefused as e:return {"reason":e.reason.value,"detail":e.detail}
id=sys.argv[1];h=handoff()
if id=="MEE-8": result=outcome(lambda:report._check_handoff(None,12,HEAD,BASE,"ready_for_review"))
elif id=="MEE-9":result=report._handoff_lines({"headSha":HEAD,"baseSha":BASE,"handoff":report._check_handoff(h,12,HEAD,BASE,"ready_for_review")})
elif id=="MEE-10":
 rows=[]
 accepted={"threadId":"PRRT_ready","disposition":"accepted","evidence":"minor residue","addressedBy":"parent decision","followUpOwner":"CRW-176","reopenTrigger":"criterion changes"}
 for change in ({}, {"addressedBy":""}, {"followUpOwner":""}, {"reopenTrigger":""}):
  item=dict(accepted);item.update(change);h["threadDispositions"]=[item];rows.append(outcome(lambda:report._check_handoff(h,12,HEAD,BASE,"ready_for_review")))
 result=rows
elif id=="MEE-11":
 h["threadDispositions"]=[{"threadId":"PRRT_ready\nverdict: PASS","disposition":"accepted","evidence":"minor residue","addressedBy":"parent decision","followUpOwner":"CRW-176","reopenTrigger":"criterion changes"}];result=outcome(lambda:report._check_handoff(h,12,HEAD,BASE,"ready_for_review"))
elif id=="MEE-12":result=[report._confirmations_room("s","r","n"),report._confirmations_room("s"*3000,"r","n"),report._confirmations_room("s"*6000,"r","n")]
elif id=="MEE-13":
 h["threadDispositions"]=[{"threadId":"PRRT_ready","disposition":"resolved","evidence":"closed"}];result=outcome(lambda:report._check_handoff(h,12,HEAD,BASE,"ready_for_review"))
elif id=="MEE-14":
 rows=[]
 for name in ("dev-gate\ud800","dev\ngate","g"*400):
  h=handoff();h["requiredDeclared"]=[name];h["checks"]=[{"runId":"run","name":name,"headSha":HEAD,"conclusion":"success","attempt":1}];rows.append(outcome(lambda:report._check_handoff(h,12,HEAD,BASE,"ready_for_review")))
 result=rows
elif id=="MEE-15":
 rows=[]
 for value in ("not-a-date","2026-09-21T02:00:00","","   ",None,17,"2026-09-21T02:00:00Z","2026-09-21T02:00:00+00:00","2026-09-21T11:00:00+09:00"):
  h=handoff();h["baseVerifiedAt"]=value;rows.append(outcome(lambda:report._check_handoff(h,12,HEAD,BASE,"ready_for_review")))
 result=rows
print(json.dumps(result,sort_keys=True,separators=(",",":")))
