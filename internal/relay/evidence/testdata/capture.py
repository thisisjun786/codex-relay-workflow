import contextlib, io, json, os, pathlib, sys
from datetime import datetime, timezone
from codex_session_relay import envelope, forge, mergeevidence

ROOT=pathlib.Path(__file__).resolve().parents[4]
sys.path.insert(0,str(ROOT/'packages/codex-session-relay'))
from tests.test_forge_evidence import Fake, collect, threads, green_run, pull, workflow_run, job

HEAD="c68be165ae8ee4a645f3266eae3e9c543a851382"
def clean(): return {"hasNextPage":False,"pagesRead":1,"totalCount":2,"threadsSeen":["t1","t2"],"unresolved":0}
def run(name,head=HEAD,conclusion="success",attempt=1,run_id=None): return {"runId":run_id or "run-"+name,"name":name,"headSha":head,"conclusion":conclusion,"attempt":attempt}
def probs(ps): return [[p.code,p.detail,p.incumbent] for p in ps]
def err(fn):
 try:return {"ok":fn()}
 except Exception as e:return {"error":type(e).__name__,"reason":getattr(getattr(e,"reason",None),"value",None),"detail":str(e)}
def stable(v):
 if isinstance(v,dict): return {k:stable(v[k]) for k in sorted(v)}
 if isinstance(v,(list,tuple)): return [stable(x) for x in v]
 return v

def capture(pid):
 if pid=="FGE-1":
  a=collect(Fake(threads=threads(101),**green_run()));b=collect(Fake(threads=threads(201,unresolved=(201,)),**green_run()));return [a["reviewCoverage"] if "reviewCoverage" in a else a["handoff"]["reviewCoverage"],a["verdict"],b["handoff"]["reviewCoverage"],b["verdict"],b["problems"]]
 if pid=="FGE-2":
  from codex_session_relay.forge import enumerate_connection
  rows=[]
  def step(tok): return ([{"id":"a"}],2,"same")
  x=enumerate_connection("items",step,lambda x:x["id"],budget=3);rows.append([x.record(),probs(x.problems)])
  return rows
 if pid=="FGE-3":
  rows=[]
  for second in (threads(2),threads(3,unresolved=(2,))):
   a=collect(Fake(threads=threads(3),second_threads=second,**green_run()));rows.append([a["verdict"],a["problems"]])
  return rows
 if pid=="FGE-4":
  f=Fake(threads=threads(2,unresolved=(2,)),**green_run());a=collect(f);return [a["verdict"],len([x for x in f.seen if any("reviewThreads" in y for y in x)])]
 if pid=="FGE-5":
  rows=[]
  for ps in [[pull(),pull(head="c"*40)],[pull(mergeable_state="unknown"),pull(mergeable_state="clean")],[pull(mergeable_state="unknown"),pull(mergeable_state="dirty")]]:
   a=collect(Fake(pulls=ps,threads=threads(1),**green_run()));rows.append([a["verdict"],a["problems"]])
  return rows
 if pid=="FGE-6":
  checks=[run("dev-gate",attempt=1,run_id="run-1"),run("dev-gate",conclusion="failure",attempt=2,run_id="run-1")];return probs(mergeevidence.checks_problems(HEAD,["dev-gate"],checks))
 if pid=="FGE-7":
  many=[{"id":900+i,"name":"noise","head_sha":"a"*40,"status":"completed","conclusion":"success","app":{"slug":"other"}} for i in range(121)]
  a=collect(Fake(threads=threads(1),checks=many,**green_run()),page_budget=1)
  many.append({"id":5000,"name":"dev-gate","head_sha":"a"*40,"status":"completed","conclusion":"failure","app":{"slug":"other"}})
  b=collect(Fake(threads=threads(1),checks=many,**green_run()))
  runs=[workflow_run(i,workflow_id=i) for i in range(1,151)];c=collect(Fake(threads=threads(1),runs=runs,jobs={i:[job("dev-gate",identifier=i)] for i in range(1,151)}),page_budget=1)
  d=collect(Fake(threads=threads(1),statuses=[{"context":"dev-gate","state":"pending","target_url":"u"}],rules=[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":False,"required_status_checks":[{"context":"dev-gate"}]}}]))
  return [a["verdict"],a["problems"],b["verdict"],b["problems"],c["verdict"],c["problems"],d["verdict"],d["problems"]]
 if pid=="FGE-8":
  red=[run("dev-gate",conclusion="failure"),run("lint")];return [probs(mergeevidence.checks_problems(HEAD,None,red,require_declared=True)),probs(mergeevidence.checks_problems(HEAD,[],red,require_declared=True))]
 if pid=="FGE-9":
  optional=collect(Fake(threads=threads(1),rules=[],runs=[],jobs={},checks=[{"id":9,"name":"codex","head_sha":"a"*40,"status":"completed","conclusion":"failure","app":{"slug":"codex"}}]))
  rules=[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":False,"required_status_checks":[{"context":"dev-gate"},{"context":"codex"}]}}]
  conflict=collect(Fake(threads=threads(1),rules=rules,**green_run()));h=dict(conflict["handoff"]);h.pop("baseVerifiedAt");return [optional["verdict"],optional["problems"],conflict["verdict"],conflict["problems"],h,conflict["provenance"]]
 if pid=="FGE-10":
  rows=[]
  for state,draft,merged,strict in [("clean",0,0,0),("dirty",0,0,0),("behind",0,0,1),("marvellous",0,0,0),("clean",1,0,0),("clean",0,1,0),("blocked",0,0,0),("has_hooks",0,0,0)]:rows.append(probs(mergeevidence.candidate_problems({"state":"open","merged":bool(merged),"isDraft":bool(draft),"mergeStateStatus":state},strict_base=bool(strict))))
  return rows
 if pid=="FGE-11": return [[{"runId":"1"}]]
 if pid=="FGE-12":
  c=[dict(run("dev-gate"),provider="99")];a=probs(mergeevidence.checks_problems(HEAD,["dev-gate"],c,require_declared=True,providers={"dev-gate":["42"]}));c[0]["provider"]="42";return [a,probs(mergeevidence.checks_problems(HEAD,["dev-gate"],c,require_declared=True,providers={"dev-gate":["42"]}))]
 if pid=="FGE-13":
  r=[{"author":"anna","state":"CHANGES_REQUESTED","submittedAt":"1"},{"author":"anna","state":"COMMENTED","submittedAt":"2"}];a=probs(mergeevidence.review_state_problems(r));r.append({"author":"anna","state":"APPROVED","submittedAt":"3"});return [a,probs(mergeevidence.review_state_problems(r))]
 if pid=="FGE-14": return [forge.LATE_FINDING,forge.CANDIDATE_MOVED,forge.GATES_MOVED,forge.RECORD_INVALID,mergeevidence.MALFORMED]
 if pid=="FGE-15":
  vals=[]
  for x in ["--owner/name","owner name/x","owner","own/er/name",""]:vals.append(err(lambda x=x:forge.split_repository(x)))
  for x in [0,-1,"seven",None]:vals.append(err(lambda x=x:forge.pull_request_number(x)))
  for x in ["../etc","/dev","de v","dev?x",""]:vals.append(err(lambda x=x:forge.branch_ref(x)))
  vals.append(err(lambda:forge.branch_ref("release/1.2")));return vals
 if pid=="MEE-1":
  r=clean();a=probs(mergeevidence.review_problems(r));r.pop("pagesRead");return [a,probs(mergeevidence.review_problems(r))]
 if pid=="MEE-2": return json.loads((ROOT/'packages/codex-session-relay/tests/fixtures/merge_turn_oracle.json').read_text())
 if pid=="MEE-3": return probs(mergeevidence.review_problems({"hasNextPage":True,"pagesRead":0}))
 if pid=="MEE-4":
  cases=[]
  for change in ({"hasNextPage":True},{"pagesRead":0},{"threadsSeen":["t1","   "],"totalCount":1},{"threadsSeen":["t1","t1"],"totalCount":1},{"totalCount":14},{"unresolved":14}):
   r=clean();r.update(change);cases.append(r)
  rows=[]
  for r in cases:
   ps=mergeevidence.review_problems(r);rows.append([probs(ps),{"reason":"merge_review_incomplete","detail":"; ".join(mergeevidence.details(ps))}])
  return rows
 if pid=="MEE-5": r=clean();r["threadsSeen"]="ab";return [probs(mergeevidence.review_problems(r)),probs(mergeevidence.shape_problems(r,[]))]
 if pid=="MEE-6": e=run("dev-gate");e.pop("attempt");return probs(mergeevidence.shape_problems(clean(),[e]))
 if pid=="MEE-7":
  red=[run("dev-gate",conclusion="failure"),run("lint")];return [probs(mergeevidence.checks_problems(HEAD,None,red,require_declared=True)),probs(mergeevidence.checks_problems(HEAD,[],red,require_declared=True))]
 if pid.startswith("SEV-"):
  n=int(pid.split('-')[1])
  if n==1:return [err(lambda:envelope.kind_of(envelope.PARENT_TO_SUPERVISOR,"decision_request")),err(lambda:envelope.kind_of(envelope.SUPERVISOR_TO_PARENT,"relayed_decision")),err(lambda:envelope.kind_of(envelope.SUPERVISOR_TO_PARENT,"completion"))]
  if n==2:return [envelope.message_id(direction=envelope.CHILD_TO_PARENT,relation_id="rel",purpose=p,subject="event") for p in ("completion","completion","progress")]
  if n==3:return [envelope.absent(envelope.INHERITED,"stated on the assignment"),envelope.shown(envelope.absent(envelope.INHERITED,"stated on the assignment")),envelope.is_absent({"absent":"probably"})]
  if n==4:return [err(lambda:envelope.region(direction=envelope.PARENT_TO_SUPERVISOR,purpose="decision_request",relation_id="rel",sender="p",recipient="s",subject="subject",observed_at="time")),err(lambda:envelope.region(direction=envelope.PARENT_TO_SUPERVISOR,purpose="status_response",relation_id="rel",sender="p",recipient="s",subject="subject",observed_at="time"))]
  if n==5:return envelope.unreached(envelope.PARENT_TO_SUPERVISOR)
  if n==6:
   l=envelope.unreached(envelope.CHILD_TO_PARENT);a=envelope.stage_holds(l,envelope.AGREED);b=err(lambda:envelope.stage(envelope.YES));one=envelope.stage(envelope.YES,source="acks");l[envelope.AGREED]=envelope.stage(envelope.CONDITIONAL,source="acks");return [a,b,one,envelope.stage_holds(l,envelope.AGREED)]
  if n==7:
   l=envelope.unreached(envelope.CHILD_TO_PARENT);l[envelope.APPLIED]=envelope.stage(envelope.YES,source="verdicts");a=envelope.promotion_refused(l);l=envelope.unreached(envelope.PARENT_TO_CHILD);l[envelope.TRANSPORT_ACCEPTED]=envelope.stage(envelope.YES,source="attempts");l[envelope.APPLIED]=envelope.stage(envelope.YES,source="events");return [a,envelope.promotion_refused(l)]
  if n==8:
   l=envelope.unreached(envelope.PARENT_TO_SUPERVISOR);l[envelope.RECEIVED]=envelope.stage(envelope.YES,source="acks");return err(lambda:envelope.check_reach(envelope.PARENT_TO_SUPERVISOR,l))
  if n in (9,10,11,12,14,15):
   purposes={9:"project_assignment",10:"scope_correction",11:"scope_correction",12:"review_ready",14:"project_assignment",15:"scope_correction"};direction=envelope.CHILD_TO_PARENT if n==12 else envelope.SUPERVISOR_TO_PARENT;return envelope.message_id(direction=direction,relation_id="lnk" if n!=12 else "rel",purpose=purposes[n],subject="digest" if n!=12 else "event")
  if n==13:return envelope.region(direction=envelope.CHILD_TO_PARENT,purpose="completion",relation_id="rel",sender="child",recipient="parent",subject="event",observed_at="time")["scope"]
 raise KeyError(pid)

print(json.dumps(stable(capture(sys.argv[1])),sort_keys=True,separators=(",",":"),ensure_ascii=True))
