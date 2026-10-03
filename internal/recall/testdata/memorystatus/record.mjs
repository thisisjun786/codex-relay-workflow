// Read-only CXC v0.2.40 oracle; all stores below are synthetic temporary files.
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
const dist = process.argv[2];
const m = await import(dist + '/memory-status.js');
const { openDbReadWrite } = await import(dist + '/sqlite.js');
const schema = 'CREATE TABLE jobs (kind, status, retry_remaining, last_error, finished_at)';
const row = (kind, status, retry, error, finished) => `INSERT INTO jobs VALUES (${[kind,status,retry,error,finished].map(x=>x===null?'NULL':typeof x==='number'?String(x):"'"+x.replaceAll("'","''")+"'").join(',')})`;
const cases = [
  {id:'missing'}, {id:'empty',sql:[schema]},
  {id:'healthy',sql:[schema,row('memory_stage1','done',3,null,1000),row('memory_stage1','done',3,null,2000),row('memory_consolidate_global','done',3,null,1500)]},
  {id:'exhausted',sql:[schema,row('memory_stage1','error',0,'context window exceeded',10),row('memory_stage1','error',0,'429 rate limit',11),row('memory_stage1','error',2,'transient',12)]},
  {id:'all-causes',sql:[schema,...['capacity','context window exceeded','incomplete response','stream closed','other',null,'capacity','other'].map((e,i)=>row('stage','error',0,e,i))]},
  {id:'no-success',sql:[schema,row('stage','running',3,null,null)]},
  {id:'no-jobs',sql:['CREATE TABLE other (x)']},
  {id:'unsupported',sql:['CREATE TABLE jobs (kind, status)']},
  {id:'corrupt',mode:'corrupt'}, {id:'open-failure',mode:'directory'},
  {id:'newest-unsupported',sql:[schema],newer:['CREATE TABLE jobs (kind, status)']},
  {id:'filters',sql:[schema,row('stage','error',-1,'quota',1),row('stage','done',0,'quota',2),row('stage','error',null,'quota',3),row('stage','error',0,null,null)]},
  {id:'dynamic',sql:[schema,'INSERT INTO jobs VALUES (NULL, 12, 0, NULL, NULL)',row('a','done',0,null,10.5),row('a','error',0,'quota',20.25)]},
  {id:'non-numeric-time',sql:[schema,row('a','done',3,null,'bogus')]},
  {id:'hex-time',sql:[schema,row('a','done',3,null,'0x10')]},
  {id:'infinite-time',sql:[schema,row('a','done',3,null,'Infinity')]},
  {id:'blob-time',sql:[schema,"INSERT INTO jobs VALUES (x'343239', 'done', 0, x'343239', x'3132')"]},
  {id:'unsafe-integer',sql:[schema,row('a','error',0,'quota',10),'INSERT INTO jobs VALUES (\'a\',\'done\',3,NULL,9007199254740992)']},
];
for(const missing of ['kind','status','retry_remaining','last_error','finished_at'])cases.push({id:'missing-'+missing,sql:['CREATE TABLE jobs ('+['kind','status','retry_remaining','last_error','finished_at'].filter(x=>x!==missing).join(',')+')']});
for(const [i,time] of ['', ' 12 ', '\u00a012\u2028', '0x10', '0b11', '0o7', '-0x1', '0x', '0x1000000000000000000000000', '1e3', '1e400', '.5', '5.', '+1', '1_000', '12abc', '1.2.3', 'Infinity', '-Infinity', 'infinity', 'inf', 'nan', '0x1p-2'].entries())cases.push({id:'number-'+i,sql:[schema,row('a','done',3,null,time)]});
for(const blob of ['', '01', '0102'])cases.push({id:'blob-'+blob,sql:[schema,"INSERT INTO jobs VALUES ('a','done',3,NULL,x'"+blob+"')"]});
const grid = [];
for (const c of cases) {
  const home=mkdtempSync(join(tmpdir(),'crw-memorystatus-'));
  try {
    for(const [name,sql] of [['memories_1.sqlite',c.sql],['memories_2.sqlite',c.newer]]) {
      if(!sql)continue;const db=openDbReadWrite(join(home,name));for(const s of sql)db.exec(s);db.close();
    }
    if(c.mode==='corrupt')writeFileSync(join(home,'memories_1.sqlite'),'this is not a database');
    if(c.mode==='directory')mkdirSync(join(home,'memories_1.sqlite'));
    const files=readdirSync(home).sort();const before=c.mode==='directory'?null:files.map(f=>readFileSync(join(home,f)));
    const status=m.collectMemoryStatus(home);
    const clean=x=>JSON.parse(JSON.stringify(x).replaceAll(home,'<HOME>').replaceAll('cxc memory status','crw recall memory status'));
    const observed=[];
    for(const now of [0,2100,4599,4600,87399,87400,173800,173801,99999999])observed.push({now,text:clean(m.formatMemoryStatus(status,now)),notice:clean(m.memoryStatusNotice(status,now)),custom:clean(m.memoryStatusNotice(status,now,100))});
    grid.push({...c,status:clean(status),observed});
    if(JSON.stringify(files)!==JSON.stringify(readdirSync(home).sort()))throw Error('oracle created files: '+c.id);
    if(before&&before.some((b,i)=>!b.equals(readFileSync(join(home,files[i])))))throw Error('oracle wrote store: '+c.id);
  } finally {rmSync(home,{recursive:true,force:true});}
}
const classify=[];
for(const raw of [null,'','CONTEXT WINDOW EXCEEDED','context length too long','context exceed quota','context alone','rate limit','quota','429','capacity','incomplete','stream closed','stream alone','close alone','other',' ',429,false])classify.push({raw,out:m.classifyMemoryError(raw)});
process.stdout.write(JSON.stringify({collect:grid,classify},null,2)+'\n');
