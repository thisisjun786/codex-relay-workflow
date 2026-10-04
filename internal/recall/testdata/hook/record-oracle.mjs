// CXC v0.2.40 (3c1459ac), hook.ts:40-220,599-788. Node recorder only.
// node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist <scratch> <output>
import {readFileSync,writeFileSync,mkdirSync} from 'node:fs';
import {join} from 'node:path';
import {createHash} from 'node:crypto';
import {pathToFileURL} from 'node:url';
const [dist,scratch,output]=process.argv.slice(2);
mkdirSync(scratch,{recursive:true});
for(const k of ['HOME','CODEX_HOME','CODEXCLAW_HOME'])process.env[k]=scratch;
process.env.CODEXCLAW_CXC='cxc';
const file=join(scratch,'hook.mjs');
writeFileSync(file,readFileSync(new URL(dist+'/hook.js'),'utf8').replace(/from "\.\/(.*?)"/g,(_,p)=>'from '+JSON.stringify(dist+'/'+p))+'\nexport {buildDirective, buildContextOutput, recoveryLine, sessionNotice, memoriesTableBody};\n');
const h=await import(pathToFileURL(file).href),rows=[];
// Apply the declared rename to emitted strings; inputs use upstream spellings.
const rename=s=>s.replaceAll('cxc-recall','crw-recall').replaceAll('cxc chat','crw recall chat').replaceAll('cxc memory','crw recall memory');
function add(kind,input,out){rows.push({kind,input,out:typeof out==='string'?rename(out):out});}
const prompts=[
'그때 그 작업 이어서 해줘','지난번에 하던 리팩토링 계속','저번 세션에서 결정한 스키마 뭐였지','예전에 만든 스크립트 찾아줘','트라이그램 인덱스 어디까지 했지?','그 플래그 기억나?',
'continue what we did last session','what did we decide about the schema?','remember when we fixed the ingest race?','as discussed earlier, ship the index','previously we capped tool output — why?',
'revert the previous commit','기억해줘','이전 작업 이어서','리콜해줘','메모리에서 찾아줘','리콜','이전작업','기억 안 나','기억하니','기억하냐','previous session','previous work','previous conversation','previous discussion','previous chat','previously discussed the cap','prior discussion',
'리콜 기능 구현해줘','메모리에서 찾는 코드를 고쳐줘','recall 훅 테스트 추가해줘','chat search UX 개선해줘','memory search 랭킹 고쳐줘','the previous time CI failed','I recall seeing this in the Node docs','prior art for this API','the previous test failed','previous commit','bump the previous version','기억해','기억해둬','기억해서 둬','remember this','메모리에 남겨줘','빌드 돌리고 테스트 고쳐줘','',
'run cxc chat search "trigram" --days 0 and summarize','use $cxc-recall on this','그때 codexclaw.mjs chat search "x"','그때 run chat search "x"','그때 CXC chat search "x"',
'이전에 했던 배포 스크립트 다시 보자','그 세션에서 정한 예산이 뭐지','prior work on ingest?','we shipped that a while ago, right?','그때 봤어','그때에도','LaSt SeSsIoN','laſt session','그때\ufeff그 작업','previous\u00a0session','previous\u0085session'
];
for(const prompt of prompts){const input=prompt.replaceAll('cxc chat','crw recall chat').replaceAll('cxc memory','crw recall memory').replaceAll('$cxc-recall','$crw-recall').replaceAll('codexclaw.mjs','old-dispatcher.mjs');
// The raw dispatcher alternative was deliberately dropped by name-substitution.
if(prompt.includes('codexclaw.mjs'))continue;
add('detect',input,h.detectRecallIntent(prompt));add('ups',{hook_event_name:'UserPromptSubmit',prompt:input},h.handleUserPromptSubmit({hook_event_name:'UserPromptSubmit',prompt}));}
for(const p of [{},{prompt:13,hook_event_name:'UserPromptSubmit'},{prompt:'지난번',hook_event_name:'Stop'}])add('ups',p,h.handleUserPromptSubmit(p));
for(const prompt of ['지난번 2.49.0 provenance와 hook.ts, 그리고 SessionStart','그때 "the ingest race" 얘기했잖아','지난번 그 작업 이어서','2.1 2.2 2.3 2.4 2.5 2.6','SessionStart hook.ts hook.ts FooBar fooBar ERR_X CRW-XYZ','"last" \'time\' `session`','"😀"','"😀a"','"'+ '😀'.repeat(30)+'"','"'+ '😀'.repeat(31)+'"','`abc`def`','2026.10.04 1.2.3.4 x.TS', '"İSTANBUL" "İstanbul"', '지난번 "last|time"', '지난번 "x\ud800y" "x\ud801y"'])for(const cap of [0,1,4,-1])add('extract',{prompt,cap},h.extractRecallTargets(prompt,cap));
for(const source of ['','startup','resume','clear','compact','other'])for(const dedicated of [false,true])for(const status of ['','4 files / 20 messages, 5 source, 1 stale, last ingest X'])add('session',{source,dedicated,status,notice:'memory pipeline: 2 exhausted'},h.handleSessionStart(status,undefined,source||undefined,{dedicatedTools:dedicated,memoryNotice:'memory pipeline: 2 exhausted'}));
for(const config of ['','[other]\nx=1\n','[memories]\ndedicated_tools = true\n\n[other]\nx=1\n','[tools]\ndedicated_tools=true\n','[memories] # comment\r\n dedicated_tools = true # comment\r\n','[memories]\ndedicated_tools = "true"\n','[memories]\ndedicated_tools = false\n[other]\ndedicated_tools = true\n','[memories]\ndedicated_tools = true\r','[memories]\ndedicated_tools = true\u2028']){writeFileSync(join(scratch,'config.toml'),config);add('config',config,h.dedicatedToolsEnabled(scratch));}
for(const ctx of ['', ' \ufeff\r\nhello\rworld\t ', '<>&\u2028\u2029', '\ud800x\udc00'])add('envelope',{ctx},h.buildContextOutput('SessionStart',ctx));
for(const input of [{prefix:32703,emoji:true,tail:100},{prefix:32768,emoji:false,tail:0},{prefix:32769,emoji:false,tail:0}]){const ctx='x'.repeat(input.prefix)+(input.emoji?'😀':'')+'z'.repeat(input.tail),out=h.buildContextOutput('SessionStart',ctx);add('envelopeLong',input,{sum:createHash('sha256').update(out).digest('hex').match(/.{8}/g),chars:out.length});}
for(const inv of ['cxc','"'+ 'long/'.repeat(45)+'cxc"'])for(const dedicated of [false,true])add('recovery',{inv:inv.replaceAll('cxc','crw'),dedicated},h.recoveryLine(inv,dedicated));
writeFileSync(output,JSON.stringify({oracle:'CXC v0.2.40 3c1459ac hook.ts:40-220,599-788',rows},null,2)+'\n');console.log(rows.length+' oracle cases');
