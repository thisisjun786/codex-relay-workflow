// Node 24 recorder for CXC v0.2.40 (3c1459ac), index-search.ts:35-143,154-271.
// Usage: node record-oracle.mjs file://<oracle>/recall/dist <scratch> [linux|darwin]
// Only the scratch copy gains private exports; the oracle remains read-only.
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
const [dist, scratch, platform = 'linux'] = process.argv.slice(2);
mkdirSync(scratch, { recursive: true });
Object.defineProperty(process, 'platform', { value: platform });
const original = readFileSync(new URL(dist + '/index-search.js'), 'utf8');
const modulePath = join(scratch, 'index-search.mjs');
writeFileSync(modulePath, original.replace(/from "(\.\/[^\"]+)"/g,
  (_, relative) => 'from "' + new URL(relative, dist + '/').href + '"') +
  '\nexport { MAX_REPO_THREAD_IDS, RELAXED_POOL, MAX_OPTIONAL_LANE_WORDS, poolSize, planPoolSize, laneQuery, ftsQuote, escapeLike, sameOriginThreadIds, wordCondition, groupCondition, candidateFilter, textMatches };\n');
const o = await import(pathToFileURL(resolve(modulePath)).href);
const idx = await import(dist + '/index-db.js');
const fold = platform !== 'linux';
const cases = [], add = (fn, args, out) => cases.push({ fn, in: args, out });
const term = (text, boundary = false) => ({ text, boundary });
const group = (...words) => words.map(w => term(w));
const plan = (required = [], optional = [], minOptional = 0, anyMode = false) => ({ required, optional, minOptional, anyMode });
const plans = [plan(), plan([group('abc')]), plan([group('ci')]),
  plan([group('abc', 'def'), group('ci')]), plan([group('abc')], [group('def')], 1, true),
  plan([], [group('abc'), group('def'), group('ghi')], 2),
  plan([[term('ci', true)]], [group('abc'), group('def')], 1), plan([[]]),
  plan([], [group('ab'), [], group('😀😀'), group('😀😀😀'), ...['abc','def','ghi','jkl','mno','pqr','stu'].map(w => group(w))], 1)];
const words = ['', 'a', 'ci', 'abc', '한글', '한글셋', '😀😀', '😀😀😀', 'a😀b', 'ü', 'ΑΣ',
  '"', 'a"b', '" OR abc OR "', "'; DROP TABLE msgs;--", '%', '_', '\\', '\\%_', 'a%c', 'a_c', 'ab\0c'];
const opts = (p = plans[1]) => ({ plan: p, limit: 20, contextN: 2, cutoffIso: null, role: null,
  cwd: null, source: 'all', includeSynthetic: true, includeTools: true, home: 'fixture-home',
  repoKey: null, repoThreadIds: [], hasRepoKeyColumn: true });
if (!fold) {
  add('constants', [], [o.MAX_REPO_THREAD_IDS,o.RRF_K,o.LANE_WEIGHT_FTS,o.LANE_WEIGHT_TRI,o.RECENCY_WEIGHT,o.RECENCY_HALF_LIFE_HOURS,o.RELAXED_POOL,o.MAX_OPTIONAL_LANE_WORDS]);
  for (const value of [-100,0,9,10,10.25,20,49,50,500,1e30,'NaN','Infinity','-Infinity']) {
    const n = typeof value === 'string' ? Number(value) : value;
    add('pool', [value], o.poolSize(n));
    for (const p of plans) add('planPool', [p,value], o.planPoolSize(p,n));
  }
  for (const p of plans) {
    add('lane', [p], o.laneQuery(p));
    for (const text of ['','ABC CI','precision','ci abc','ci def','abc def','Ü','ΑΣ','😀😀😀']) add('text', [text,p], o.textMatches(text,p));
  }
  for (const word of words) {
    add('quote', [word], [o.ftsQuote(word),o.escapeLike(word)]);
    const params = []; add('word', [word], [o.wordCondition(word,params),params]);
  }
  for (const g of [[], ...words.map(w => group(w)), group('ci','abc','a%b')]) {
    const params = ['preceding']; add('group', [g], [o.groupCondition(g,params),params]);
  }
  const entries = [['z',{gitOriginUrl:'git@example.test:org/repo.git'}],['other',{gitOriginUrl:'https://example.test/elsewhere'}],
    ['a',{gitOriginUrl:'https://EXAMPLE.test/org/repo/'}],['missing',{gitOriginUrl:null}]];
  for (const key of [null,'','example.test/org/repo','example.test/elsewhere']) add('origin', [entries,key], o.sameOriginThreadIds({byId:new Map(entries)},key));
  const big = Array.from({length:5003},(_,i)=>['t'+i,{gitOriginUrl:'git@example.test:org/repo.git'}]);
  const ids = o.sameOriginThreadIds({byId:new Map(big)},'example.test/org/repo');
  add('originCap', [], [ids.length,ids[0],ids.at(-1)]);
}
const seeds = {
  files: [['repo',1,1,'t0','/repo','main','2026-01-01',null],['child',1,1,'t1','/repo/sub','main','2026-01-01',null],
    ['neighbour',1,1,'t2','/repo2','main','2026-01-01',null],['remote',1,1,'t3','/elsewhere','main','2026-01-01','example.test/org/repo'],
    ['sub',1,1,'t4','/repo','subagent','2026-01-01',null],['wild',1,1,'t5','/re%_po/sub','main','2026-01-01',null],
    ['nearwild',1,1,'t6','/reXYpo/sub','main','2026-01-01',null],['drive',1,1,'t7','\\\\?\\c:\\Repo\\child\\','main','2026-01-01',null],
    ['unc',1,1,'t8','\\\\?\\UNC\\server\\share\\child','main','2026-01-01',null],['upper',1,1,'t9','/Ü/child','main','2026-01-01',null]],
  msgs: [[1,'repo',0,'2026-01-02','user','content',0,'abc ci'],[2,'child',0,'2026-01-03','assistant','content',0,'def abc'],
    [3,'neighbour',0,'2025-01-01','user','content',0,'abc'],[4,'remote',0,'2026-01-04','user','content',0,'abc def'],
    [5,'repo',1,'2026-01-02','user','content',1,'abc injected'],[6,'repo',2,'2026-01-02','tool','tool_output',0,'abc tool'],
    [7,'sub',0,'2026-01-02','user','content',0,'abc subagent'],[8,'wild',0,'2026-01-02','user','content',0,'abc'],
    [9,'nearwild',0,'2026-01-02','user','content',0,'abc'],[10,'drive',0,'2026-01-02','user','content',0,'abc'],
    [11,'unc',0,'2026-01-02','user','content',0,'abc'],[12,'repo',3,'2026-01-02','user','content',0,'Ü'],
    [13,'repo',4,'2026-01-02','user','content',0,'ü'],[14,'repo',5,'2026-01-02','user','content',0,'a%c a_c \\%_'],
    [15,'repo',6,'2026-01-02','user','content',0,'abc OR def'],[16,'repo',7,'2026-01-02','user','content',0,'ab\0c'],
    [17,'repo',8,'2026-01-02','user','content',0,"'; DROP TABLE msgs;--"],[18,'repo',9,'2026-01-02','user','content',0,'precision'],
    [19,'upper',0,'2026-01-02','user','content',0,'abc']]
};
const db = idx.openIndex(join(scratch,'index.sqlite'));
const putFile=db.prepare('INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES(?,?,?,?,?,?,?,?)');
const putMsg=db.prepare('INSERT INTO msgs(id,path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?,?,?,?,?,?)');
seeds.files.forEach(r=>putFile.run(...r)); seeds.msgs.forEach(r=>putMsg.run(...r));
const candidates = [];
for (const p of plans) for (const withWords of [false,true]) candidates.push([opts(p),withWords]);
for (const [key,values] of Object.entries({source:['all','main','subagent',''],role:['user','assistant',"' OR 1=1 --"],
  cutoffIso:['2026-01-03',"' OR 1=1 --"],cwd:['/repo','/repo/','/re%_po','c:\\Repo','\\\\server\\share','/ü',"' OR 1=1 --"]}))
  for (const value of values) candidates.push([{...opts(),[key]:value},true]);
for (const synthetic of [false,true]) for (const tools of [false,true]) candidates.push([{...opts(),includeSynthetic:synthetic,includeTools:tools},true]);
for (const word of words) candidates.push([opts(plan([group(word)])),true]);
candidates.push([{...opts(),cwd:'/repo',repoKey:'example.test/org/repo'},true],
  [{...opts(),cwd:'/repo',repoThreadIds:['t3',"' OR 1=1 --"]},true],
  [{...opts(),cwd:null,repoKey:'example.test/org/repo',repoThreadIds:['t3']},true]);
for (const legacy of [false,true]) {
  if (legacy) db.exec('DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key');
  for (const [base,withWords] of candidates) {
    const q={...base,hasRepoKeyColumn:!legacy};
    const {where,params}=o.candidateFilter(q,withWords);
    add('filter', [q,withWords,fold], [where,params]);
    let result;
    try {
      const rows=db.prepare('SELECT m.id,m.text FROM msgs m JOIN files f ON f.path=m.path WHERE '+where+' ORDER BY m.id').all(...params);
      result={ids:rows.map(r=>r.id),hits:rows.filter(r=>o.textMatches(r.text,q.plan)).map(r=>r.id),error:null};
    } catch(e) { result={ids:[],hits:[],error:e.message}; }
    add('candidates', [q,withWords,fold,legacy], result);
  }
}
db.close();
process.stdout.write(JSON.stringify({platform,seeds,cases})+'\n');
