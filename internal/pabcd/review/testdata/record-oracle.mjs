// Node 24 recorder. Usage: node record-oracle.mjs <oracle-tree> <output.json>
import {writeFileSync,mkdirSync,mkdtempSync} from 'node:fs';
import {pathToFileURL} from 'node:url';
import {resolve,dirname,join} from 'node:path';
const tree=process.argv[2], output=process.argv[3];
const rr=await import(pathToFileURL(resolve(tree,'plugins/codexclaw/components/pabcd-state/dist/review-round.js')));
const fixed='2026-01-01T00:00:00.000Z', RealDate=Date;
globalThis.Date=class extends RealDate { constructor(...args){super(...(args.length?args:[fixed]));} static now(){return new RealDate(fixed).getTime();} };
const base=()=>({objective:'review round fixture',slug:'review-round-fixture',createdAt:fixed,updatedAt:fixed,activeWorkPhaseId:null,workPhases:[],criteria:[],host:{armed:false,armedAt:null,source:'none'}});
const round=(id,status='pending',purpose='plan_audit',extra={})=>({roundId:id,purpose,planPath:'plan.md',planSha256:'hash-a',status,lane:{launchId:id+'-launch'},openedAt:fixed,...extra});
const cases=[];
function add(name,op,args){cases.push({name,op,args,expected:rr[op](...args)});}
const messages=['','LAUNCH: id\nVERDICT: PASS','laUnch : id\r\n verdict : fail\r\n','LAUNCH: id\nVERDICT: NEAR-PASS','LAUNCH: id\nVERDICT: GO-WITH-FIXES','LAUNCH: id\nVERDICT: GO-WITH-FIXES (blockers=0)','LAUNCH: id\nVERDICT: PASS\ntrailer','LAUNCH: id\nVERDICT: paß','LAUNCH: id\nVERDICT: paſſ','LAUNCH: id\nVERDICT: faıl','LAUNCH: id\nVERDICT: GO-WITH-ﬁXES','LAUNCH: a\u0085b\nVERDICT: PASS','LAUNCH\u0085: id\nVERDICT: PASS','LAUNCH\ufeff:\ufeffid\nVERDICT: PASS','LAUNCH\u2028: id\nVERDICT: PASS','LAUNCH: a\u2029b\nVERDICT: PASS','LAUNCH: id\nVERDICT: PA\rSS','LAUNCH: id\nVERDICT: PA\u2028SS','LAUNCH: id\nVERDICT: PA\u2029SS','LAUNCH: id\nVERDICT: ＰＡＳＳ','LAUNCH: id extra\nVERDICT: PASS','LAUNCH: id\n\nVERDICT: PASS\n\n'];
for(let i=0;i<messages.length;i++)add('signoff-'+i,'parseSignoff',[messages[i]]);
for(const id of ['r01','junk','r+10tail','r-2tail','r \ufeff8rest','r9007199254740992','r100000000000000000000','r999999999999999999999','r1'+'0'.repeat(309),'R9','rr8'])add('order-'+id.slice(0,25),'openRound',[{...base(),reviewRounds:[round(id,'approved')]},{purpose:'plan_audit',planPath:'plan.md',planSha256:'hash-b',now:()=>fixed}]);
// Function-valued now is omitted in JSON; the fixture's openedAt is supplied by the frozen clock.
const p={...base(),reviewRounds:[round('r1'),round('r2','in_flight')],activePlanAuditRoundId:'r1'};
add('cursor-reuse','openRound',[p,{purpose:'plan_audit',planPath:'plan.md',planSha256:'hash-b'}]);
add('cursor-abort','abortRound',[p,'plan_audit','stop']);
add('launch-omitted-workspace','markLaunching',[{...base(),reviewRounds:[round('r1')]},'plan_audit','r1','r1-launch']);
add('duplicate-id','openRound',[{...base(),reviewRounds:[round('r9007199254740992','approved')]},{purpose:'plan_audit',planPath:'plan.md',planSha256:'hash-b'}]);
for(const verdict of ['pass','near-pass','fail'])add('verdict-'+verdict,'recordVerdict',[{...base(),reviewRounds:[round('r1','in_flight')]},{purpose:'plan_audit',roundId:'r1',launchId:'r1-launch',verdict,artifactSha256:'',reviewerSession:'',sourceIdentity:{kind:'resolved',commitSha:'c',dirty:false,capturedAt:fixed,treeHash:''}}]);
for(const status of ['pending','launching','in_flight','approved','changes_requested','inconclusive'])add('epoch-'+status,'supersedeStaleRounds',[{...base(),reviewRounds:[round('r1',status,'plan_audit',{ownerSessionId:'s',planEpoch:'old'}),round('r2','in_flight','final_gate',{ownerSessionId:'s',planEpoch:'old'}),round('r3','in_flight','plan_audit',{ownerSessionId:'other',planEpoch:'old'}),round('r4','in_flight','plan_audit',{ownerSessionId:'s',planEpoch:'new'})],activePlanAuditRoundId:'r4'},'plan_audit','s','old']);
add('epoch-no-owner','supersedeStaleRounds',[{...base(),reviewRounds:[round('r1','in_flight','plan_audit',{planEpoch:'old'})]},'plan_audit','','old']);
const gp=await import(pathToFileURL(resolve(tree,'plugins/codexclaw/components/pabcd-state/dist/goalplan.js')));
for(const owner of [null,'']) {
 const cwd=mkdtempSync(join(dirname(output),'revive-'));
 const p={...base(),reviewRounds:[round('r1','in_flight','plan_audit',{planEpoch:'old',...(owner===null?{}:{ownerSessionId:owner})})]};
 const dir=gp.goalplanDir(cwd,p.slug);mkdirSync(dir,{recursive:true});writeFileSync(join(dir,'goalplan.json'),JSON.stringify(p));
 const revived=gp.readGoalplan(cwd,p.slug);
 if(!revived)throw Error('owner fixture revival failed');
 cases.push({name:'revived-owner-'+(owner===null?'absent':'empty'),op:'revivedOwner',args:[p],expected:{owner:revived.reviewRounds[0].ownerSessionId??null,closed:rr.supersedeStaleRounds(revived,'plan_audit','','old').closed}});
}
writeFileSync(output,JSON.stringify(cases,null,2)+'\n');
console.log(cases.length+' oracle cases recorded');
