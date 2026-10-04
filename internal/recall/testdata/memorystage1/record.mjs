// Node24 development recorder for CXC v0.2.40 (3c1459ac) memory-search.ts.
// node record.mjs file://<oracle-dist> <scratch> > oracle.json
// The oracle is read-only; synthetic inputs are rebuilt by the Go replay.
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
const { searchMemory } = await import(process.argv[2] + '/memory-search.js');
const root = fs.mkdtempSync(path.join(process.argv[3], 'stage1-'));
const now = Date.parse('2026-07-06T00:00:00Z'), day = 86400000;
const cases = [];
const add = (name, query, rows = [], options = {}, extra = {}) => cases.push({name, query, rows, options: {nowMs: now, ...options}, ...extra});
const row = (id, raw, summary = '', sec = now / 1000) => [id, raw, summary, sec];
add('db-only', 'quagga', [row('db', 'quagga migrations', 'quagga summary')]);
add('md-dedupe', 'trigram', [row('main', 'trigram raw')], {}, {files: {'rollout_summaries/a.md': 'thread_id: main\n\ntrigram deployment'}});
const scopeRows = [row('t1', 'Handbook consolidated in scoped project')];
const scopeExtra = {files: {'rollout_summaries/a.md': 'thread_id: t1\ncwd: /proj/other\n\n# Memory Handbook\n\nNo groups consolidated yet.'}, threads: [['t1', '/proj/here', null]], legacy: true};
add('rejected-file-retains-db', 'Handbook consolidated', scopeRows, {cwd:'/proj/here',cwdOnly:true}, scopeExtra);
add('retained-file-dedupes-db', 'Handbook consolidated', scopeRows, {cwd:'/proj/here'}, scopeExtra);
add('row-fields-and', 'quagga migration', [row('both', 'quagga', 'migration'), row('one','quagga')]);
add('any', 'quagga wombat', [row('one','quagga'), row('two','wombat')], {any:true});
add('optional-quorum', 'alpha beta gamma delta epsilon zeta theta omega 문제', [row('four','alpha beta gamma delta'),row('two','alpha beta')], {synonyms:false});
for (const synonyms of [true,false]) add('synonyms-'+synonyms,'배포',[row('deploy','deployment')],{synonyms});
add('required-sql', 'LSP quagga', [row('yes','LSP quagga'),row('no','quagga')]);
add('sql-wildcards', 'ab_cd', [row('literal','ab_cd'),row('wildcard','abXcd')]);
add('sql-quote', "quagga' OR 1=1", [row('no','quagga')], {synonyms:false});
add('unicode-like-kept', 'ü', [row('upper','Ü')]);
add('scalar-body', '123', [['number',123,null,null]]);
add('blob-body', '97', [], {}, {schema: "CREATE TABLE stage1_outputs(thread_id,raw_memory,rollout_summary,source_updated_at); INSERT INTO stage1_outputs VALUES ('blob', X'6162',NULL,NULL)"});
add('blob-body-positive', 'ab 97', [], {any:true}, {schema: "CREATE TABLE stage1_outputs(thread_id,raw_memory,rollout_summary,source_updated_at); INSERT INTO stage1_outputs VALUES ('blob', X'6162',NULL,NULL)"});
add('null-empty-id-crlf', 'quagga', [row(null,'quagga\r\nnotes','',null),row('','quagga')]);
add('nonstring-id', 'quagga', [row(42,'quagga')]);
add('invalid-date-partial', 'quagga', [row('fresh','quagga','',now/1000),row('bad','quagga','',-1e13)]);
add('extended-iso', 'quagga', [row('far','quagga','',253402300800)]);
add('fractional-second', 'quagga', [row('fraction','quagga','',1.2349)]);
add('age-cutoff', 'quagga', [row('old','quagga','',(now-day-1)/1000),row('equal','quagga','',(now-day)/1000),row('null','quagga','',null),row('text','quagga','','0')], {days:1});
add('zero-cutoff-kept','quagga',[row('old','quagga','',-86400)],{nowMs:day,days:1});
add('boundary-relaxed','3956',[row('embedded','PR3956 shipped')]);
add('presence-blocks-relax','3956 LSP',[row('embedded','LSP PR3956'),row('presence','3956 without the other symbol')]);
add('old-presence-ignored','3956 LSP',[row('embedded','LSP PR3956'),row('old','3956','',(now-2*day)/1000)],{days:1});
add('scope-presence-blocks-relax','3956 LSP',[row('here','LSP PR3956'),row('other','3956')],{cwd:'/proj/here',cwdOnly:true},{threads:[['here','/proj/here',null],['other','/proj/other',null]]});
add('db-error-repeated','3956',[],{}, {schema:'CREATE TABLE other(value)'});
add('db-corrupt','quagga',[],{}, {corrupt:true});
add('scope-origin','quagga',[row('same','quagga'),row('other','quagga')],{cwd:'/worktrees/task',cwdOnly:true},{threads:[['same','/proj/main','https://github.com/example/alpha.git'],['other','/proj/other','https://github.com/example/beta.git']],origin:'git@github.com:example/alpha.git'});
add('scope-prose','quagga',[row('mentioned','quagga /proj/here-adjacent')],{cwd:'/proj/here',cwdOnly:true});
add('scope-empty','quagga',[row('unknown','quagga')],{cwd:'/proj/here',cwdOnly:true});
const chatHit = (text='pangolin migration', ts='2026-07-05T00:00:00.000Z') => ({ts,role:'user',text,matchField:'content',threadId:'chat-one',title:null,cwd:'/proj/here',gitBranch:null,source:'main',file:'session.jsonl',context:[]});
for (const [name,options] of [['empty',{}],['opts',{days:3,any:true,synonyms:false,chatIncludeTools:true}],['hard',{cwd:'/proj/here',cwdOnly:true}],['soft',{cwd:'/proj/here'}]]) add('fallback-'+name,'pangolin',[],options,{chat:[chatHit()]});
add('fallback-none','pangolin',[],{}, {noChat:true});
add('fallback-empty-retains','pangolin',[],{chatFallbackBelow:2},{chat:[],files:{'MEMORY.md':'pangolin thin'}});
add('fallback-sufficient','pangolin',[],{}, {chat:[chatHit()],files:{'MEMORY.md':'pangolin thin'}});
add('fallback-threshold-replaces','pangolin',[],{chatFallbackBelow:2},{chat:[chatHit()],files:{'MEMORY.md':'pangolin thin'}});
add('fallback-error-retains','pangolin',[],{chatFallbackBelow:2},{chatError:'index unavailable',files:{'MEMORY.md':'pangolin thin'}});
add('fallback-cap','pangolin',[],{}, {chat:Array.from({length:7},(_,i)=>chatHit('pangolin '+i))});
add('fallback-fraction','pangolin',[],{limit:1.5},{chat:[chatHit('one'),chatHit('two'),chatHit('three')]});
add('fallback-clip','pangolin',[],{}, {chat:[chatHit('😀'.repeat(210),'not-a-date')]});
add('fallback-ignores-chat-warning','pangolin',[],{}, {chat:[chatHit()],chatWarning:'child warning'});
add('empty-query-no-chat',' \ufeff\t',[],{}, {chat:[chatHit()]});
for(let i=0;i<cases.length;i++){
  const c=cases[i],home=path.join(root,'c'+i);fs.mkdirSync(path.join(home,'memories'),{recursive:true});
  for(const [name,content] of Object.entries(c.files??{})){const p=path.join(home,'memories',name);fs.mkdirSync(path.dirname(p),{recursive:true});fs.writeFileSync(p,content);fs.utimesSync(p,new Date(now),new Date(now));}
  const dbPath=path.join(home,'memories_1.sqlite');
  if(c.corrupt)fs.writeFileSync(dbPath,'not sqlite');else{const db=new DatabaseSync(dbPath);db.exec(c.schema??'CREATE TABLE stage1_outputs(thread_id,raw_memory,rollout_summary,source_updated_at)');if(c.rows.length){const ins=db.prepare('INSERT INTO stage1_outputs VALUES(?,?,?,?)');for(const r of c.rows)ins.run(...r);}db.close();}
  if(c.threads){const db=new DatabaseSync(path.join(home,'state_1.sqlite'));db.exec("CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT,cwd TEXT,git_branch TEXT,"+(c.legacy?'':'git_origin_url TEXT,')+'updated_at_ms INTEGER)');const ins=db.prepare('INSERT INTO threads VALUES('+ (c.legacy?'?,?,?,?,?':'?,?,?,?,?,?')+')');for(const [id,cwd,origin] of c.threads)ins.run(...(c.legacy?[id,'',cwd,null,0]:[id,'',cwd,null,origin,0]));db.close();}
  c.calls=[];
  const searchChat=c.noChat||(!c.chat&&!c.chatError)?undefined:(_q,o)=>{const {readOriginUrl,...call}=o;c.calls.push({...call,home:'$HOME'});if(c.chatError)throw new Error(c.chatError);return {hits:c.chat??[],warnings:c.chatWarning?[c.chatWarning]:[],scannedFiles:0,matchedFiles:0,totalFiles:0,elapsedMs:0,mode:'index'};};
  c.out=searchMemory(c.query,{...c.options,home,readOriginUrl:()=>c.origin??null,searchChat});c.out.elapsedMs=0;
}
process.stdout.write(JSON.stringify(cases,null,2)+'\n');
