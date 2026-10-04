// CXC v0.2.40 3c1459ac. Runs selected original query tests unchanged except
// import redirects into a recording wrapper. Usage: node record-oracle.mjs
// <read-only source oracle root> <read-only recording oracle root> <scratch>.
// stdout is generated testdata; no oracle write and no Node at Go test time.
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
const [source, recording, scratch] = process.argv.slice(2);
const root = path.join(source, 'plugins/codexclaw/components/pabcd-state');
const dist = pathToFileURL(path.join(recording, 'plugins/codexclaw/components/pabcd-state/dist')).href;
fs.mkdirSync(scratch, {recursive: true});
process.env.TMPDIR=path.join(scratch,'tmp');fs.mkdirSync(process.env.TMPDIR,{recursive:true});
const names = ['remainingWorkPhases', 'readyWorkPhases', 'readyTasks', 'nextOpenTask', 'dependencyWaitReasons', 'dependencyDeadlock', 'remainingWorkAwaitsDecisions', 'openDecisionIdsForPhase'];
fs.writeFileSync(path.join(scratch, 'capture.mjs'), `export const cases=[];export let current='';export function test(name,fn){current=name;fn();}export function capture(fn,args,expected){const plan=structuredClone(args[0]);plan.createdAt=plan.updatedAt='2026-01-01T00:00:00.000Z';cases.push({test:current,fn,plan,phase:args[1]?structuredClone(args[1]):undefined,expected:structuredClone(expected)});}`);
fs.writeFileSync(path.join(scratch, 'wrapper.mjs'), `export * from '${dist}/goalplan.js';import * as g from '${dist}/goalplan.js';import {capture} from './capture.mjs';\n` + names.map(n => `export function ${n}(...a){const v=g.${n}(...a);capture('${n}',a,v);return v;}`).join('\n'));
for (const file of ['goalplan.test.ts', 'work-phase-states.test.ts', 'goalplan-regression.test.ts']) {
  let text = fs.readFileSync(path.join(root, 'test', file), 'utf8');
  text = text.replace(/^test\([\s\S]*?^\}\);/gm, body => names.some(n => new RegExp('\\b'+n+'\\(').test(body)) ? body : '');
  text = text.replace('from "node:test"', 'from "./capture.mjs"').replace('from "../src/goalplan.ts"', 'from "./wrapper.mjs"')
    .replace(/from "\.\.\/src\/([^".]+)\.ts"/g, (_, name) => `from "${dist}/${name}.js"`)
    .replace('from "../test-support/symlink-support.ts"', `from "${pathToFileURL(path.join(root, 'test-support/symlink-support.ts')).href}"`);
  text = text.replace('join(here, "fixtures", "goalplans-pre-change-baseline.json")', JSON.stringify(path.join(root, 'test/fixtures/goalplans-pre-change-baseline.json')));
  const target = path.join(scratch, file);
  fs.writeFileSync(target, text);
  await import(pathToFileURL(target).href);
}
const {cases} = await import(pathToFileURL(path.join(scratch, 'capture.mjs')).href);
const g = await import(dist + '/goalplan.js');
const phase = (id, more={}) => ({id, title:id, status:'pending', tasks:[], criteriaIds:[], ...more});
const task = (id, more={}) => ({id, title:id, status:'pending', ...more});
const plan = (more={}) => ({...g.buildGoalplan({objective:'query edge', now:()=> '2026-01-01T00:00:00.000Z'}), ...more});
const decision = (status='open') => ({id:'d',question:'Choose',status,askedAt:'2026-01-01T00:00:00.000Z',...(status==='decided'?{answer:'yes',decidedAt:'2026-01-01T01:00:00.000Z'}:{})});
const extra = [];
const add = (test, p) => extra.push({test, plan:p});
add('empty plan',plan());
add('first phase match and mismatched next pair',plan({workPhases:[phase('dup',{status:'done'}),phase('dup',{tasks:[task('later')]}),phase('leaf',{dependsOn:['dup']})]}));
add('first task match and stable dedup',plan({workPhases:[phase('p',{dependsOn:['z','z','a'],tasks:[task('dup'),task('dup',{status:'done'}),task('leaf',{dependsOn:['dup','gone','dup']})]})]}));
for (const status of ['blocked','superseded','done','pending','in_progress']) add('phase status '+status,plan({workPhases:[phase('p',{status,blockedReason:'',supersededBy:'replacement',awaitsDecision:['d']}),phase('replacement',{status:'done'})],decisions:[decision()]}));
for (const decisions of [[],[decision()],[decision('decided')],[decision(),decision('decided')],[decision('decided'),decision()]]) add('decision identity '+JSON.stringify(decisions.map(x=>x.status)),plan({workPhases:[phase('p',{awaitsDecision:['d','d','ghost']})],decisions}));
add('task-only dependency cycle',plan({workPhases:[phase('p',{tasks:[task('a',{dependsOn:['b']}),task('b',{dependsOn:['a']})]})]}));
add('phase cycle',plan({workPhases:[phase('a',{dependsOn:['b']}),phase('b',{dependsOn:['a']})]}));
add('blocked hides dependency in deadlock',plan({workPhases:[phase('p',{status:'blocked',blockedReason:'vendor',dependsOn:['gone'],awaitsDecision:['d'],tasks:[task('a',{dependsOn:['missing']})]})],decisions:[decision()]}));
add('met criterion with blank JS evidence',plan({workPhases:[phase('p',{awaitsDecision:['d']})],decisions:[decision()],criteria:[{id:'c',scenario:'proof',expectedEvidence:'',capturedEvidence:'\uFEFF',status:'met'}]}));
add('met criterion with NEL evidence',plan({workPhases:[phase('p',{awaitsDecision:['d']})],decisions:[decision()],criteria:[{id:'c',scenario:'proof',expectedEvidence:'',capturedEvidence:'\u0085',status:'met'}]}));
add('future schema',plan({schemaVersion:4,workPhases:[phase('p',{awaitsDecision:['d']})],decisions:[decision()]}));
add('done phase with pending task',plan({workPhases:[phase('p',{awaitsDecision:['d']}),phase('done',{status:'done',tasks:[task('open')]})],decisions:[decision()]}));
add('open decision on blocked dependency plus closable phase',plan({workPhases:[phase('blocked',{status:'blocked',awaitsDecision:['d']}),phase('leaf',{dependsOn:['blocked']}),phase('closable',{tasks:[task('done',{status:'done',outcome:'proof'})]})],decisions:[decision()]}));
for(const schemaVersion of [undefined,2,3])for(const fixtureTask of [task('t',{status:'done'}),task('t',{outcome:'premature'}),task('t',{status:'done',outcome:'tests: 12 passed'})])add('regression outcome fixture '+(schemaVersion??1)+' '+fixtureTask.status+' '+(fixtureTask.outcome??'missing'),plan({schemaVersion,workPhases:[phase('wp1',{status:fixtureTask.status==='done'?'done':'in_progress',tasks:[fixtureTask]})]}));
const selected = cases.slice();
for(const {test,plan:p} of [...selected,...extra]) {
 for(const fn of names.filter(x=>x!=='openDecisionIdsForPhase'))cases.push({test,fn,plan:p,expected:g[fn](p)});
 for(const wp of p.workPhases)cases.push({test,fn:'openDecisionIdsForPhase',plan:p,phase:wp,expected:g.openDecisionIdsForPhase(p,wp)});
}
process.stdout.write(JSON.stringify({oracle:'CXC v0.2.40 3c1459ac',selectedOriginalCalls:selected.length,selectedOriginalTests:[...new Set(selected.map(x=>x.test))],cases})+'\n');
