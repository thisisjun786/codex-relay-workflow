"""Behavioral mutation proof for remaining todo-23A properties; serial byte restore."""
import pathlib
import subprocess
import tempfile
import sys

root=pathlib.Path(__file__).resolve().parents[4]
evidence=pathlib.Path(os.environ.get('CRW_EVIDENCE_PATH', root/'.omo/evidence/task-23-crw-go-port.txt'))
R='internal/relay/routing/'
F='internal/relay/faults/'
mutations=[
('PR-1',R+'placement.go','return covering[0]["ref"], nil,','return registry["triageProject"], nil,','project'),
('PR-2',F+'ledger.go','Degraded: int64(3)','Degraded: int64(2)','suppression.threshold'),
('PR-3',F+'leases.go',"last_error=COALESCE(last_error,'the lease expired after the write was issued')","last_error=COALESCE(last_error,'the lease expired before the write was issued')",'last_error'),
('PR-4',R+'completion_record.go','"claimed_severity": "degraded"','"claimed_severity": "notice"','incident_routes.claimed_severity'),
('PR-5',R+'completion_record.go','is bound with no project and %s has','is bound without a project and %s has','refusal.detail'),
('PR-6',R+'completion_record.go','occurred again after the fix ','occurred again before the fix ','recurrences.reason'),
('PR-7',R+'project_publication.go','len(criteria) > 1','len(criteria) > 2','skipped/queued'),
('PR-8',R+'projects.go','" is a non-empty list of names"','" is a non-empty list of identifiers"','refusal.detail'),
('PR-9',R+'project_publication.go','"components": sortedKeys(object(group["components"]))','"components": sortedKeys(object(group["members"]))','payload.components'),
('PR-10',R+'reconcile.go','if held && unreached[key] != nil {','if held && unreached[key] == nil {','decisions'),
('PR-11',R+'obligations.go','return "link_incomplete"','return "cause_unverified"','attention'),
('PR-12',R+'intake.go','if origin != incident["origin"] {','if origin == incident["origin"] {','unverifiedCause'),
('PR-13',R+'completion_record.go','"recorded": "replayed"','"recorded": "recorded"','recorded'),
('PR-14',R+'obligations.go','makeOwed("reopen", nil, nil, nil)','makeOwed("add_label", nil, nil, "unexpected")','obligation.kind'),
('PR-15',R+'intake.go','no fault of that product with that signature is recorded here','no fault of that product with that signature is available here','unverifiedCause.why'),
('PR-16',R+'intake.go','"replayed": len(stored)','"replayed": len(stored)+1','replayed'),
('PR-17',R+'reconcile.go','"proposalsUnreached": unreached','"proposalsUnreached": unreached+1','proposalsUnreached'),
('PR-18',R+'project_publication.go','Creates: true, Target: "team",','Creates: true, Target: "team+project",','publication.target'),
('PR-19',R+'reconcile.go','held["hold"], held["owner"] = "owner_found_after_create"','held["hold"], held["owner"] = "ambiguous_owner"','route.target.hold'),
('PR-20',R+'router.go','before["workspace"] != registry["workspace"]','before["workspace"] == registry["workspace"]','registry/refusal'),
('PR-21',R+'products.go','project in RESERVED_PROJECTS','unused','unused'),
('PR-22',R+'router.go',('''	err := r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		product := object(value)["product"]''','''		encoded, err := Canonical(binding)'''),('''	product := object(value)["product"]''','''	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		encoded, err := Canonical(binding)'''),'transactionReads.binding'),
('PR-23',R+'obligations.go','contradicted = o["toIssue"] != nil && !contains(decision["relate"], o["toIssue"])','contradicted = o["toIssue"] != nil && contains(decision["relate"], o["toIssue"])','target.obligations'),
('PR-24',R+'intake.go','"recorded": false, "publication": nil','"recorded": true, "publication": nil','recorded'),
('PR-25',R+'project_publication.go','RegisterKind("project_create",','RegisterKind("project_create_changed",','project_create'),
('PR-26',R+'intake.go',('''		if err != nil {
			_, rollback := db.ExecContext(ctx, "ROLLBACK TO route_occurrence")''','''	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		answer, err = r.intake(ctx, incident)
		return err
	})
	return answer, err'''),('''		if err == nil {
			_, rollback := db.ExecContext(ctx, "ROLLBACK TO route_occurrence")''','''	var bodyErr error
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		answer, bodyErr = r.intake(ctx, incident)
		return nil
	})
	if bodyErr != nil { err = bodyErr }
	return answer, err'''),'tables.fault_ledger.length (failed filing committed)'),
('PR-27',R+'placement.go','return owned("attach_current", attached','return owned("accumulate", attached','disposition'),
('PRD-10',R+'router.go','; register it first','; register the product first','refusal.detail'),
('PRD-13',R+'router.go','this checkout lacks','this checkout needs','refusal.detail'),
('PRD-16',R+'cli.go','file cannot be read: ','file could not be read: ','refusal.detail'),
('PC-1','internal/relay/registry/linkage_cli.go','return reading("unregistered", true','return reading("unregistered", false','readable'),
('PC-2','internal/relay/registry/linkage_cli.go','return reading("complete_candidate", true','return reading("complete", true','state'),
('PC-3','internal/relay/registry/linkage_cli.go','if len(owners) > 1 {','if len(owners) > 2 {','state'),
('PC-4','internal/relay/registry/linkage_cli.go','return reading("incomplete", true','return reading("complete_candidate", true','state'),
]
# Reserved scope's own validation branch, not a shared reason constant.
mutations[20]=('PR-21',R+'products.go','if p == ProjectsScope {','if p == ProjectsScope+"different" {','refusal/detail')
# Move the actual registry read/validation outside Compose, not just its observer hook.
binding_source=(root/R/'router.go').read_text()
binding_start=binding_source.index('\terr := r.Store.Compose',binding_source.index('func (r *Router) Bind('))
binding_end=binding_source.index('\t\tencoded, err := Canonical(binding)',binding_start)
binding_before=binding_source[binding_start:binding_end]
binding_after=binding_before.split('\n',1)[1].replace('return err','return nil, err').replace('return routeRefused','return nil, routeRefused')+'\terr = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {\n'
mutations[21]=('PR-22',R+'router.go',binding_before,binding_after,'transactionReads.binding')
with tempfile.TemporaryDirectory(prefix='routing-integration-mutations-') as scratch:
 with evidence.open('a') as log:
  log.write('\n### part A integration mutations\nCommand: python3 internal/relay/routing/testdata/integration_mutations.py\nBase e5b10415\n')
  for identifier,name,before,after,field in mutations:
   if len(sys.argv)>1 and identifier not in sys.argv[1:]:continue
   path=root/name;original=path.read_bytes()
   edits=list(zip(before,after)) if isinstance(before,tuple) else [(before,after)]
   mutated=original
   lines=[]
   for old,new in edits:
    count=original.count(old.encode())
    # PR-2 changes the degraded threshold at read and at escalation together.
    assert count==1 or identifier=='PR-2' and count==2,(identifier,name,count,old)
    lines.append(original[:original.index(old.encode())].count(b'\n')+1)
    mutated=mutated.replace(old.encode(),new.encode())
   backup=pathlib.Path(scratch)/path.name;backup.write_bytes(original)
   line=','.join(map(str,lines))
   try:
    path.write_bytes(mutated)
    family,number=identifier.split('-')
    result=subprocess.run(['go','test','./internal/relay/routing','-run',f'^Test23_{family}_{number}_','-count=1'],cwd=root,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=180)
    output=result.stdout.decode()
    if result.returncode!=1 or 'differ' not in output or 'panic:' in output or 'build failed' in output:
     raise RuntimeError(f'invalid proof {identifier}: exit={result.returncode}\n{output}')
    log.write(f'{identifier}: {name}:{line}; before={before!r}; after={after!r}; first field={field}; test exit 1.\n')
    start=output.index(next(x for x in output.splitlines() if 'differ' in x));end=output.find('    --- FAIL:',start)
    log.write(output[start:end if end!=-1 else len(output)]+'\n');log.flush();print(identifier+': killed',flush=True)
   finally:
    path.write_bytes(backup.read_bytes());subprocess.run(['cmp',str(path),str(backup)],check=True)
  log.write('All integration mutations restored by byte copy + cmp.\n')
