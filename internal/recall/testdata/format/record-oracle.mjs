// Node 24 recorder for CXC v0.2.40 (3c1459ac) recall format.ts and cli.ts:29-145.
// node record-oracle.mjs <oracle-root> <scratch-root> > oracle.json (TZ=UTC).
// Only scratch copies are changed: type-only imports removed and private exports appended.
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [root,scratch]=process.argv.slice(2), src=path.join(root,'plugins/codexclaw/components/recall/src');
fs.mkdirSync(scratch,{recursive:true});
const format=fs.readFileSync(path.join(src,'format.ts'),'utf8').replace(/^import type.*\n/gm,'');
fs.writeFileSync(path.join(scratch,'format.ts'),format+'\nexport { clip,chatHitHeader,topicTokens,sharesTopic,clipField };\n');
const cli=fs.readFileSync(path.join(src,'cli.ts'),'utf8').split('\n').slice(28,145).join('\n');
fs.writeFileSync(path.join(scratch,'flags.ts'),'import {parseArgs} from "node:util"; import {existsSync} from "node:fs"; const DEFAULT_DAYS=7, DEFAULT_LIMIT=50, DEFAULT_MEMORY_LIMIT=20;\n'+cli+'\nexport {USAGE,wantsHelp,flagLikePathError,readFlags,explicitHome,parseFlags,numFlag};\n');
const f=await import(pathToFileURL(path.join(scratch,'format.ts'))), c=await import(pathToFileURL(path.join(scratch,'flags.ts')));
const rows=[], now=Date.parse('2026-09-10T00:00:00Z'), day=86400000;
const add=(fn,input,run)=>{const r={fn,in:input};try{r.out=run();if(r.out===undefined)r.out=null;}catch(e){r.error=e.message;}rows.push(r);};
const mh=(relpath,excerpt,days,extra={})=>({origin:'file',kind:'handbook',threadId:null,startLine:1,cwd:null,score:1,relpath,excerpt,updatedAt:days===null?null:new Date(now-days*day).toISOString(),...extra});
const mr=(hits,extra={})=>({hits,warnings:[],scannedFiles:hits.length,elapsedMs:1,...extra});
const older=mh('rollout_summaries/old.md','2.49.0 provenance check',14,{score:9}), newer=mh('MEMORY.md','2.49.0 provenance verified',1,{score:3});
const freshness=[mr([mh('MEMORY.md','no stamp here',null)]),mr([older,newer]),mr([mh('a.md','alpha notes about the intake form',30),mh('b.md','beta notes about the outbox',2)]),mr([mh('MEMORY.md','2.49.0 provenance check',9),mh('MEMORY.md','2.49.0 provenance rerun',1,{startLine:40})]),mr([mh('MEMORY.md','wp6 recall labels',2,{startLine:12,cwd:'/repo/current'})],{warnings:['index is stale'],scannedFiles:4,elapsedMs:7}),mr([])];
freshness.push(mr([mh('odd.md','x'.repeat(301),null,{startLine:null,updatedAt:'not a date'})],{warnings:['one','two'],elapsedMs:.25}));
for(const r of freshness)add('memory',[r,now],()=>f.formatMemoryResult(r,now));
for(const date of [new Date(now-3*day).toISOString(),new Date(now-3*day-1000).toISOString(),new Date(now).toISOString(),new Date(now+5*day).toISOString(),null,'not a date','', '2026','2026-09','2026-09-08','2026-02-30','2026-02-32','2026-09-08T24:00:00Z','2026-09-08T24:00:01Z','2026-09-08T00:00Z','2026-09-08T00:00:00.123456Z','2026-09-08T00:00:00+0900','2026-09-08T00:00:00+09:00','2026-09-08T00:00:00','2026-09-08t00:00:00z','Tue, 08 Sep 2026 00:00:00 GMT','2026/9/8','09/08/2026','0','1','13','31','32','49','50','99','100','0000','2026-9-8',' 2026-09-08 '])add('age',[date,now],()=>f.ageDays(date,now));
for(const hits of [freshness[1].hits,freshness[2].hits,freshness[3].hits,[older,mh('newest.md','2.49.0 update',0),newer],[older,newer,mh('tied.md','2.49.0 update',1)],[mh('old.md','CommonCamel 2.3 file.ts',4),mh('new.md','CommonCamel',1)],[mh('old.md','plain words',4),mh('new.md','plain words',1)],[{...older,updatedAt:'bad'},newer]])for(let i=0;i<hits.length;i++)add('newer',[hits[i],hits],()=>f.newerRelpath(hits[i],hits));
for(const s of ['', 'x'.repeat(299),'x'.repeat(300),'x'.repeat(301),'x'.repeat(299)+'😀', '한글\t문장\n  정리', '\uFEFFa\u00A0b\u0085c\u180Ed\u200Be','\t \n'])for(const cap of [0,60,200,300])add('clip',[s,cap],()=>f.clip(s,cap));
for(const excerpt of ['1.2 1.2.3 1.2.3.4','foo.ts x.tsx foo.TS src/hook.ts','CamelCase HTTP2 CommonCamel ordinary','éFile.ts-AlphaBeta 한글FooBar','same.md same.md']){const hit=mh('notes.md',excerpt,1);add('topics',[hit],()=>[...f.topicTokens(hit)]);}
const ch={ts:'2026-09-08T00:00:00Z',role:'assistant',text:'one\n two',matchField:'content',threadId:'synthetic-thread',title:null,cwd:null,gitBranch:null,source:'main',file:'rollout.jsonl',context:[]};
const cr=(hits,warnings=[])=>({hits,warnings,scannedFiles:3,matchedFiles:2,totalFiles:4,elapsedMs:7,mode:'scan'});
for(const hit of [ch,{...ch,matchField:'tool_log',source:'subagent',title:'title '+ 'x'.repeat(70),cwd:'/repo/current'},{...ch,title:'',cwd:''},{...ch,context:[{ts:'a',role:'user',text:'x'.repeat(201),isMatch:false},{ts:'b',role:'assistant',text:'matched',isMatch:true}]}]){add('header',[hit],()=>f.chatHitHeader(hit));add('chat',[cr([hit],['index warning'])],()=>f.formatChatResult(cr([hit],['index warning'])));}
add('chat',[cr([])],()=>f.formatChatResult(cr([])));
for(const field of ['text','title','context'])for(const n of [0,499,500,501]){const hit={...ch,context:[]};if(field==='context')hit.context=[{ts:'a',role:'user',text:'x'.repeat(n),isMatch:true}];else hit[field]='x'.repeat(n);const r=cr([hit]);add('json',[r],()=>f.clipChatResultForJson(r));}
for(const args of [[],['help'],['/?'],['--help'],['-h'],['--','--help'],['--help=x'],['query','--days','0','--context=1','--any'],['-d7','-l','2','-c1'],['-d=7'],['-dl2'],['--limit='],['--limit','-2'],['--limit=-2'],['--cwd','--json'],['--cwd','-'],['--cwd=-x'],['--home=--json'],['--cwd-only'],['--days'],['--json=false'],['--what'],['-x'],['-al2'],['query','--','-x'],['--days','1','--days','2'],['--source','--help'],['--','-h'],['-','query'],['-d','-1'],['-d'],['--what=x'],['--json='],['--days','--'],['-d-5'],['--days=--x'],['-é'],['-😀'],['---x'],['--=x']]){add('help',[args],()=>c.wantsHelp(args));add('parse',[args],()=>c.parseFlags(args));}
for(const key of ['cwd','cwd-only','home','index-path'])for(const value of ['-','-x','--json','', '.', '/tmp/synthetic','-<>&','-\u2028\u2029','-\\u2028', 1])add('path',[{[key]:value}],()=>c.flagLikePathError({[key]:value}));
for(const raw of [null,false,0,'',' ','\uFEFF 4.5 \u00A0','0','-0','-1','2.99','1e2','0x10','0b11','0o10','Infinity','NaN','1_000','0x1p2','1e309','+0x10','foo'])add('num',[{limit:raw},'limit'],()=>c.numFlag({limit:raw},'limit'));
for(const args of [['--cwd=-x'],['--home=--json'],['--index-path','-'],['help','--cwd','.'],['--cwd','-x']]){const original=process.stderr.write;let stderr='';process.stderr.write=(s)=>{stderr+=s;return true;};try{const out=c.readFlags(args);rows.push({fn:'read',in:[args],out,stderr});}finally{process.stderr.write=original;}}
add('usage',[],()=>c.USAGE.replaceAll('cxc chat','crw recall chat').replaceAll('cxc memory','crw recall memory'));
const dateEdges=['+275760-09-13T00:00:00.000Z','+275760-09-13T00:00:00.001Z','-271821-04-20T00:00:00.000Z','-271821-04-19T23:59:59.999Z','-000000-01-01T00:00:00Z','+010000-01-01T00:00:00Z','-000001-01-01T00:00:00Z','2026-09-08T24:00:00.0001Z','2026-09-08T24:00:00.000Z','2026-09-08T24:00Z','2026-09-08T00:00:00+24:00','2026-09-08T00:00:60Z','2026-09-08T00:00:00.0009Z','2026-09-08T00:00:00.0019Z','2026/2/30','2/30/2026','2026-9-8',' 2026-09-08 ','2026-09-08Z','2026-09-08T00:00:00','2026-09-08','100','0099','Sept 8, 2026','September 8 2026','8 Sep 2026','Tue, 08 Sep 2026 00:00:00 EST','Tue, 08 Sep 2026 00:00:00 PST','Tue Sep 08 2026 00:00:00 GMT+0000 (Coordinated Universal Time)','2026-9-8 12:00'];
const originalTZ=process.env.TZ;
for(const zone of ['UTC','Asia/Seoul','America/New_York']){process.env.TZ=zone;for(const date of dateEdges)add('age',[date,now,zone],()=>f.ageDays(date,now));}
process.env.TZ=originalTZ;
for(const row of rows) { const wire=JSON.stringify(row.out);if(/\\u[dD][89aAbB][0-9a-fA-F]{2}/.test(wire)){row.classification='intentionally-changed';row.reason='Go UTF-8 represents a UTF-16 slice boundary lone surrogate as U+FFFD; the Node output remains recorded verbatim.';} }
process.stdout.write(JSON.stringify(rows,null,2)+'\n');
