// CXC v0.2.40 (3c1459ac), Node 24. Inputs/output only; oracle trees stay read-only.
// node record-oracle.mjs <oracle-root> <owned-scratch> > oracle.json
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [root,scratch]=process.argv.slice(2),src=path.join(root,'plugins/codexclaw/components/recall');
fs.mkdirSync(scratch,{recursive:true});
const ix=await import(pathToFileURL(path.join(src,'src/index-search.ts')));
const {openIndex}=await import(pathToFileURL(path.join(src,'src/index-db.ts')));
const {ingest}=await import(pathToFileURL(path.join(src,'src/ingest.ts')));
const {chatMatchPlan}=await import(pathToFileURL(path.join(src,'src/chat-search.ts')));
const fixture=await import(pathToFileURL(path.join(src,'test/fixtures.ts')));
const now=Date.parse('2026-09-09T00:00:00Z');Date.now=()=>now;
const line=o=>JSON.stringify(o)+'\n';
const message=(text,ts,role='user')=>line({timestamp:ts,type:'response_item',payload:{type:'message',role,content:[{type:role==='assistant'?'output_text':'input_text',text}]}});
const rankFiles={};
for(const [i,hours,text,role] of [[1,720,'quokka deployment plan for the quokka rollout, quokka everywhere'],[2,12,'unrelated standup notes that happen to name quokka once among many other words here'],[3,1,'another long unrelated message about pipelines, caches, and a quokka reference at the end'],[4,1440,'wombat wombat'],[5,2,'wombat wombat'],[6,480,'트라이그램 색인 트라이그램 재구축 트라이그램 완료'],[7,3,'한글 문서에서 트라이그램 이야기를 잠깐 언급했다','assistant']]){
 const ts=new Date(now-hours*3600000).toISOString(),id='019f0000-0000-7000-8000-0000000f000'+i;
 rankFiles[`sessions/${ts.slice(0,10).replaceAll('-','/')}/rollout-${ts.slice(0,10)}T00-00-00-${id}.jsonl`]=line({timestamp:ts,type:'session_meta',payload:{id,timestamp:ts,cwd:'/proj/rank',originator:'codex-tui'}})+message(text,ts,role);
}
const dateShapes=['2026-09-09T00:00:00.000Z','2026/09/09','2026-09-08T24:00:00Z','2026-08-32T12:00:00Z','2026-08-31T24:00:00Z','2026-09-09t00:00:00z','Wed, 09 Sep 2026 00:00:00 GMT','','not a date','2026-02-30T12:00:00Z','+010000-01-01T00:00:00Z'];
const dateFiles={'sessions/2026/09/09/dates.jsonl':line({type:'session_meta',payload:{id:'dates',cwd:'/proj/dates'}})+dateShapes.map(ts=>message('date witness',ts)).join('')};
const edgeFiles={'sessions/2026/09/09/edge.jsonl':line({type:'session_meta',payload:{id:'edge',cwd:'/proj/edge'}})+message('before context',new Date(now-3000).toISOString())+message('# AGENTS.md instructions injected',new Date(now-2000).toISOString())+line({timestamp:new Date(now-1000).toISOString(),type:'response_item',payload:{type:'function_call_output',output:'tool neighbor'}})+message('한글문서 needle needle',new Date(now).toISOString())+message('after context',new Date(now+1000).toISOString())+message('needle',new Date(now+2000).toISOString())};
const clean=(r,home)=>JSON.parse(JSON.stringify(r).split(home).join('$R'));
const corpora=[];
function corpus(name,files,specs){
 const home=fs.mkdtempSync(path.join(scratch,'home-'));
 if(name==='fixture')fixture.buildCodexHome(home);
 for(const [p,data]of Object.entries(files)){const dest=path.join(home,p);fs.mkdirSync(path.dirname(dest),{recursive:true});fs.writeFileSync(dest,data);}
 const cases=[];
 for(const spec of specs){const db=openIndex(path.join(home,'index-'+cases.length+'.sqlite'));ingest(home,db,0);
  const opts={plan:chatMatchPlan(spec.query,spec.any??false,spec.synonyms??false),limit:50,contextN:0,cutoffIso:null,role:null,cwd:null,source:'main',includeSynthetic:false,includeTools:true,home,nowMs:now,...spec.options};
  const entry={name:spec.name??spec.query,query:spec.query,options:clean(opts,home),sql:spec.sql??[],special:spec.special??{}};
  for(const[k,v]of Object.entries(entry.special))opts[k]=Number(v);
  try{for(const sql of entry.sql)db.exec(sql);if(spec.closed)db.close();const out=ix.queryIndex(db,opts);out.elapsedMs=0;entry.out=clean(out,home);}catch(e){entry.error=e.message;}finally{if(!spec.closed)db.close();}
  entry.closed=!!spec.closed;cases.push(entry);
 }
 corpora.push({name,files,cases});
}
corpus('rank',rankFiles,['quokka','wombat','트라이그램','한글'].flatMap(query=>['relevance','recent'].map(order=>({query,options:{order}}))).concat([{query:'quokka',options:{limit:1}},{query:'quokka',options:{limit:1.5}},{query:'quokka',options:{limit:1.5,order:'recent'}},{query:'quokka',options:{limit:10.25}}]));
corpus('dates',dateFiles,[{query:'date'},{query:'date',special:{nowMs:'NaN'}},{query:'date',options:{order:'recent',contextN:1}}]);
corpus('edge',edgeFiles,[{query:'한글',name:'empty-lanes',options:{contextN:2,includeTools:false}},{query:'needle',name:'missing-fts',sql:['DROP TABLE msgs_fts']},{query:'한글',name:'missing-both',sql:['DROP TABLE msgs_fts','DROP TABLE msgs_tri']},{query:'needle',options:{contextN:1,includeSynthetic:true}},{query:'needle',options:{contextN:0.5}},{query:'needle',closed:true},{query:'needle',sql:['DROP TABLE msgs']},...['relevance','recent'].flatMap(order=>[-2,-1,0,0.5,1.5,10.25].map(limit=>({query:'needle',options:{order,limit}}))),...['relevance','recent'].flatMap(order=>['NaN','Infinity'].map(limit=>({query:'needle',options:{order},special:{limit}})))]);
corpus('fixture',{},[
 ['trigram korean',{source:'all'}],['trigram korean',{source:'all'},true],['trigram',{}],['trigram',{source:'subagent'}],['트라이그램',{}],['zebra',{role:'user'}],['zebra',{role:'user',includeSynthetic:true}],['zebra-in-tool-output',{}],['zebra-in-tool-output',{includeTools:false}],['ancient question',{}],['trigram',{source:'all',cwd:'/proj/alpha'}],['한글 트라이그램 결과',{contextN:1}],['please deploy the trigram index for korean search extra',{source:'all'}],['please deploy trigram index korean search 확인 완료 없는단어',{source:'all'}]
].flatMap(([query,options,any])=>['recent','relevance'].map(order=>({query,options:{...options,order},any}))));
const rrf=[null,0,1,60,-1].flatMap(rank=>[1,.8,0].map(weight=>({rank,weight,out:ix.rrfScore(rank===null?undefined:rank,weight)})));
const recency=[null,now,now+500*3600000,now-168*3600000,now-336*3600000].map(ts=>({ts,now,out:ix.recencyScore(ts,now)}));
console.log(JSON.stringify({now,rrf,recency,corpora},null,2));
