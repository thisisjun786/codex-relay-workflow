// Record CXC v0.2.40 scan-cli.ts:211-417 and the matching readiness tests.
// Usage: node record-scan.mjs <oracle pabcd-state directory> <output.json>
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
const root = process.argv[2];
const load = name => import(pathToFileURL(path.join(root, 'dist', name + '.js')));
const scan = await load('scan-cli');
const state = await load('state');
const ledger = await load('interview-ledger');
const interview = await load('interview');
const run = (...argv) => ({kind:'scan', argv});
const qa = (id, question, answers, turn='t1') => ({kind:'qa', id, question, answers, turn});
const patch = value => ({kind:'patch', value});
const map = ['--derive', ...['goal','constraint','success','ontology'].flatMap((d,i)=>['--map',`q${i}=${d}`])];
const fullQA = () => ['goal','constraint','success','ontology'].map((d,i)=>qa(`q${i}`,`what about ${d}?`, [`answer ${d}`]));
const raw = (row) => ({kind:'raw', row});
const cases = [
  ['raw-numeric-answer', [raw({event:'question_asked',eventId:'q',questionId:'q0',question:'Goal?'}),raw({event:'answer_recorded',eventId:'a',questionId:'q0',answers:[1,1,'ok']}),run('--derive','--map','q0=goal')]],
  ['raw-numeric-question', [raw({event:'question_asked',eventId:'q',questionId:'q0',question:3}),run('--derive','--map','q0=goal')]],
  ['raw-coercion-error', [raw({event:'question_asked',eventId:'q',questionId:{toString:3},question:'Bad?'}),run('--derive','--map','q0=goal')]],
  ['raw-number-format', [raw({event:'question_asked',eventId:'q',questionId:1.0,question:'One?'}),raw({event:'answer_recorded',eventId:'a',questionId:1,answers:['one']}),run('--derive','--map','1=goal')]],
  ['huge-counts', [run('--contradictions','1000000000000000000000','--high','-0')]],
  ['raw-id-values', [raw({event:'question_asked',eventId:'q',questionId:3,question:'Numeric?'}),raw({event:'answer_recorded',eventId:'a',questionId:3,answers:['yes']}),raw({event:'question_asked',eventId:'q2',question:'Absent?'}),raw({event:'answer_recorded',eventId:'a2',answers:['missing']}),raw({event:'question_asked',eventId:'q3',questionId:null,question:'Null?'}),raw({event:'answer_recorded',eventId:'a3',questionId:null,answers:['null']}),run('--derive','--map','3=goal','--map','undefined=constraint','--map','null=success')]],
  ['raw-reference-answers', [raw({event:'question_asked',eventId:'q',questionId:'q0',question:'Objects?'}),raw({event:'answer_recorded',eventId:'a',questionId:'q0',answers:[{}, {}, [],[], false,null]}),run('--derive','--map','q0=goal')]],
  ['raw-reference-ids', [raw({event:'question_asked',eventId:'q',questionId:['a','b'],question:'Array?'}),raw({event:'answer_recorded',eventId:'a',questionId:['a','b'],answers:['unpaired']}),raw({event:'question_asked',eventId:'q2',questionId:{},question:'Object?'}),run('--derive','--map','a,b=goal','--map','[object Object]=success')]],

  ['map-order', [run('--derive','--map','qb=success','--map','qa=goal','--map','2=constraint','--map','1=ontology','--map','qb=constraint')]],
  ['max-kept', [run(),patch({dimensions:{goal:{level:'max',known:[],unknown:[],confidence:1},constraint:{level:'low',known:[],unknown:[],confidence:0},success:{level:'low',known:[],unknown:[],confidence:0},ontology:{level:'low',known:[],unknown:[],confidence:0}}}),run('--unknown','goal=gap')]],
  ['empty-question', [qa('q0','',['answer']),run('--derive','--map','q0=goal')]],
  ['blank-answer', [qa('q0','Goal?',['']),run('--derive','--map','q0=goal')]],
  
  ['help', [{kind:'help'}]],
  ['fresh', [run('--contradictions','2','--high','1')]],
  ['monotonic', [run(),run('--contradictions','3')]],
  ['manual-text', [run('--known','goal=endpoint=https://x/y?a=b:c'),run('--known','goal=second','--unknown','success=measure?')]],
  ['dimension-confidence', [run('--dim','goal=high','--confidence','constraint=0.5')]],
  ['derive', [qa('q0','Goal?', ['ship']),qa('q2','Measure?',[]),run('--derive','--map','q0=goal','--map','q2=success')]],
  ['explicit-wins', [qa('q0','Goal?',['ship']),run('--derive','--map','q0=goal','--dim','goal=mid')]],
  ['prototype-unmapped', [qa('toString','hostile?',[]),qa('hasOwnProperty','also?',[]),qa('q0','Goal?',['fine']),run('--derive','--map','q0=goal')]],
  ['retire-gap', [qa('q0','Goal?',[]),run('--derive','--map','q0=goal'),qa('q0','Goal?',['ship'],'t2'),run('--derive','--map','q0=goal')]],
  ['answers-merge', [qa('q0','Q?',['first']),qa('q0','Q?',['second','first'],'t2'),run('--derive','--map','q0=goal')]],
  ['known-cap', [...Array.from({length:60},(_,i)=>qa(`q${i}`,`Q${i}?`,[`fact-${i}`])),run('--derive',...Array.from({length:60},(_,i)=>['--map',`q${i}=goal`]).flat()),run('--derive',...Array.from({length:60},(_,i)=>['--map',`q${i}=goal`]).flat())]],
  ['unknown-cap', [...Array.from({length:60},(_,i)=>qa(`q${i}`,`Q${i}?`,[])),run('--derive',...Array.from({length:60},(_,i)=>['--map',`q${i}=goal`]).flat()),run('--derive',...Array.from({length:60},(_,i)=>['--map',`q${i}=goal`]).flat())]],
  ['assertion-persists', [run('--dim','goal=high','--dim','constraint=mid'),run('--contradictions','2','--high','1'),run('--known','ontology=entity')]],
  ['no-map-warning', [qa('q0','Goal?',['ship']),run('--derive')]],
  ['wrong-map-warning', [qa('q0','Goal?',['ship']),run('--derive','--map','absent=goal')]],
  ['last-question-text', [qa('q0','first?',[]),qa('q1','second?',[]),qa('q0','revised?',[],'t2'),run('--derive','--map','q0=goal','--map','q1=goal')]],
  ['length-only-touch', [run(...Array.from({length:50},(_,i)=>['--known',`goal=fact-${i}`]).flat(),'--dim','goal=low'),qa('q0','Goal?',['another']),run('--derive','--map','q0=goal')]],
  ['real-ready', [...fullQA(),run(...map)]],
  ['typed-not-ready', [run(...['goal','constraint','success','ontology'].flatMap(d=>['--known',`${d}=x`]))]],
  ['unanswered-not-ready', [...fullQA().slice(0,3),qa('q3','what about ontology?',[]),run(...map)]],
  ['extra-fact-ready', [...fullQA(),run(...map),run('--known','goal=extra')]],
  ['missing-provenance', [...fullQA().slice(0,3),run('--derive','--map','q0=goal','--map','q1=constraint','--map','q2=success'),run('--known','ontology=asserted')]],
  ['max-ready', [run('--known','goal=x'),patch({dimensions:Object.fromEntries(['goal','constraint','success','ontology'].map(d=>[d,{level:'max',known:['k'],unknown:[],confidence:1}]))})]],
  ['contradiction-blocks', [...fullQA(),run(...map),patch({contradictions:[{contradictionId:'c1',severity:'high',summary:'conflict'}]})]],
  ['assumption-blocks', [...fullQA(),run(...map),patch({assumptions:[{id:'a1',text:'unrecorded',recorded:false}]})]],
];
const recorded=[];
for (const [id,actions] of cases) {
  const cwd=fs.mkdtempSync(path.join(os.tmpdir(),'scan-record-'));
  const results=[];
  try {
    for (const a of actions) {
      if(a.kind==='scan'||a.kind==='help') {
        const args=scan.parseScanCliArgs(a.kind==='help'?['help']:['record','--session','s1',...a.argv],cwd);
        if('error' in args) throw new Error(args.error);
        results.push(scan.runScanCli(args));
      } else if(a.kind==='raw') {
        fs.mkdirSync(path.join(cwd,'.codexclaw','interviews'),{recursive:true});
        fs.appendFileSync(path.join(cwd,'.codexclaw','interviews','s1.jsonl'),JSON.stringify(a.row)+'\n');
      } else if(a.kind==='rawState') {
        fs.mkdirSync(path.join(cwd,'.codexclaw','sessions'),{recursive:true});
        fs.writeFileSync(path.join(cwd,'.codexclaw','sessions','s1.json'),JSON.stringify(a.value));
      } else if(a.kind==='qa') {
        ledger.captureInterviewAnswers({cwd,sessionId:'s1',turnId:a.turn,
          toolInput:JSON.stringify({questions:[{id:a.id,question:a.question}]}),
          toolResponse:JSON.stringify({answers:{[a.id]:{answers:a.answers}}})});
      } else {
        const s=state.readState(cwd,'s1');
        state.writeState(cwd,{...s,interview:{...s.interview,...a.value}});
      }
    }
    const tracker=state.readState(cwd,'s1').interview;
    const rows=state.readInterviewEvents(cwd,'s1').map(({ts,...row})=>row);
    const mapOrders=rows.map(row=>Object.keys(row.map??{}));
    recorded.push({id,actions,expect:{results,tracker,rows,mapOrders,gate:interview.evaluateInterviewGate(tracker,{backedDimensions:ledger.dimensionsBackedByAnswers(cwd,'s1')}),shapeGate:interview.evaluateInterviewGate(tracker)}});
  } finally { fs.rmSync(cwd,{recursive:true,force:true}); }
}
const verdictCwd=fs.mkdtempSync(path.join(os.tmpdir(),'scan-verdict-loss-'));
try {
 fs.mkdirSync(path.join(verdictCwd,'.codexclaw','sessions'),{recursive:true});
 const original={...state.defaultState('s1'),unverifiedSubagents:[null]};
 fs.writeFileSync(path.join(verdictCwd,'.codexclaw','sessions','s1.json'),JSON.stringify(original));
 const result=scan.runScanCli(scan.parseScanCliArgs(['record','--session','s1'],verdictCwd));
 recorded.push({id:'unverified-record-data-loss',classification:'intentionally-changed',reason:'Refuse a state rewrite that discards stored unverified records; reuse existing cliVerdictsIntact.',oracle:{result,recordsAfter:JSON.parse(fs.readFileSync(path.join(verdictCwd,'.codexclaw','sessions','s1.json'))).unverifiedSubagents}});
}finally{fs.rmSync(verdictCwd,{recursive:true,force:true});}
const cwd=fs.mkdtempSync(path.join(os.tmpdir(),'scan-loss-'));
try {
  fs.mkdirSync(path.join(cwd,'.codexclaw','sessions'),{recursive:true});
  fs.writeFileSync(path.join(cwd,'.codexclaw','sessions','s1.json'),'{"phase":"I","interview":');
  const result=scan.runScanCli(scan.parseScanCliArgs(['record','--session','s1'],cwd));
  recorded.push({id:'unreadable-state-data-loss',classification:'intentionally-changed',reason:'Refuse replacement of an unreadable state file; preserve existing bytes and ledger.',oracle:{result,stateReplaced:fs.readFileSync(path.join(cwd,'.codexclaw','sessions','s1.json'),'utf8')!=='{"phase":"I","interview":'}});
}finally{fs.rmSync(cwd,{recursive:true,force:true});}
fs.writeFileSync(process.argv[3],JSON.stringify(recorded,null,2)+'\n');
