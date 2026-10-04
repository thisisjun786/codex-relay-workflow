// Record synthetic cwd-context answers from read-only CXC v0.2.40 (3c1459ac).
// node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist <scratch>
import { mkdtempSync, mkdirSync, writeFileSync, symlinkSync } from 'node:fs';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { execFileSync } from 'node:child_process';
const cwd = await import(process.argv[2] + '/cwd-context.js');
const index = await import(process.argv[2] + '/index-db.js');
const root = mkdtempSync(join(process.argv[3], 'cwd-oracle-'));
process.env.HOME = root;
process.env.CODEX_HOME = root;
process.env.CODEXCLAW_HOME = root;
const cases = [];
function list(input) {
 const home = join(root, String(cases.length)); mkdirSync(home, { recursive: true });
 const path = join(home, 'index.sqlite');
 if (input.mode !== 'missing') {
  const db = index.openIndex(path);
  for (const f of input.files ?? []) {
   db.prepare('INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES(?,0,0,?,?,?,?,?)')
    .run(f.path, f.threadId ?? null, f.cwd ?? '/repo', f.source ?? 'main', f.date ?? '2026-01-02', f.repoKey ?? null);
   (f.msgs ?? []).forEach((m, ord) => db.prepare('INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?, ?,?,?,?)')
    .run(f.path, ord, 'ts', m.role ?? 'user', 'content', m.synthetic ?? 0, m.text));
  }
  if (input.legacy) db.exec('DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key');
  if (input.mode === 'noMsgs') db.exec('DROP TABLE msgs');
  db.close();
  if (input.mode === 'broken') writeFileSync(path, 'not sqlite');
 }
 if (input.threads) {
  const db = new DatabaseSync(join(home, 'state_2.sqlite'));
  db.exec('CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT,cwd TEXT,git_branch TEXT,git_origin_url TEXT,updated_at_ms INTEGER)');
  for (const [id, origin] of input.threads) db.prepare('INSERT INTO threads VALUES(?, ?, ?,NULL,?,0)').run(id, '', '/other', origin);
  db.close();
 }
 const opts = { indexPath: path, home, readOriginUrl: () => input.origin ?? null };
 if ('limit' in input) opts.excerptChars = input.limit;
 const out = cwd.listCwdSessions(input.cwd ?? '/repo', input.n ?? 5, opts);
 const row = { kind: 'list', in: input, out, wire: out?.map(s => JSON.stringify(s.excerpt)) ?? null };
 if (input.platformCase) {
  const code = "Object.defineProperty(process,'platform',{value:'darwin'}); const {listCwdSessions}=await import("+JSON.stringify(process.argv[2]+'/cwd-context.js')+"); process.stdout.write(JSON.stringify(listCwdSessions("+JSON.stringify(input.cwd)+",5,{indexPath:"+JSON.stringify(path)+",home:"+JSON.stringify(home)+",readOriginUrl:()=>null})));";
  row.foldedOut = JSON.parse(execFileSync(process.execPath,['--input-type=module','-e',code],{encoding:'utf8'}));
 }
 cases.push(row);
}
const common = [
 { path: 'b', threadId: 'one', msgs: [{text:'<recommended_plugins> harness'}, {text:'human opener'}] },
 { path: 'a', threadId: 'two', msgs: [{text:'older opener'}] },
 { path: 'c', threadId: 'same', cwd:'/checkout', repoKey:'example.test/group/repo', msgs:[{text:'same origin'}] },
 { path: 'd', threadId: 'foreign', cwd:'/other', repoKey:'example.test/group/other', msgs:[{text:'foreign origin'}] },
 { path: 'z', source:'subagent', msgs:[{text:'subagent'}] },
 { path: 'old', date:'2026-01-01', msgs:[{text:'old date'}] },
];
for (const query of [{}, {cwd:'/absent'}, {cwd:'/repo/sub'}, {cwd:'/other'}, {n:1}, {n:2}, {n:0}, {n:-1}, {cwd:''},
 {origin:'git@example.test:group/repo.git'}, {origin:'https://example.test/group/other.git'},
 {legacy:true, origin:'https://example.test/group/repo.git', threads:[['same','git@example.test:group/repo.git'],['foreign','https://example.test/group/other.git']]},
 {origin:'https://example.test/group/repo.git', threads:[['foreign','git@example.test:group/repo.git']]},
 {mode:'missing'}, {mode:'broken'}, {mode:'noMsgs'}]) list({files:common,...query});
const prefixes=['<recommended_plugins>', '<hook_prompt', '<realtime_delegation>', '<codex_internal_context', '<in-app-browser-context', '<send_user_message_question_reply>', '# Files mentioned by the user:', '# Files pasted by the user:', '# Browser comments:', '## Referenced chats with Codex:', '<environment_context>', '<ENVIRONMENT_CONTEXT>', '<skill>', '<subagent_notification>', '<turn_aborted>', '<permissions instructions>', '<INSTRUCTIONS>', '<user_instructions>', '<system-reminder>', '# AGENTS.md instructions', '## Workspace Context', '[Recent Context]'];
for (const prefix of prefixes) list({files:[{path:'p', msgs:[{text:' \t\ufeff'+prefix+' harness'}, {text:'actual user'}]}]});
for (const text of ['', ' \t\ufeff', '\u0085<recommended_plugins> human', '<RECOMMENDED_PLUGINS> human', 'a\u00a0b\u1680c\u2003d\u2028e\u2029f\u202fg\u205fh\u3000i\ufeffj', 'a\u0085b\u180ec\u200bd', 'first\nsecond '+ 'x'.repeat(200)]) list({files:[{path:'p', msgs:[{text}]}]});
for (const limit of [-10,-1,0,1,2,3,4,5,6,7,10,100,1000]) list({limit,files:[{path:'p',msgs:[{text:'😀abc def'}]}]});
for (const count of [3,4,5]) list({files:[{path:'p',msgs:[...Array.from({length:count},()=>({text:'<hook_prompt harness'})),{text:'later human'}]}]});
list({files:[{path:'p', msgs:[{text:'assistant',role:'assistant'}, {text:'flagged',synthetic:1}, {text:'user'}]}]});
for (const [recorded, query] of [['\\\\?\\C:\\proj\\here','C:\\proj\\here'],['//?/UNC/server/share/','//server/share'],['C:\\proj\\here','c:\\proj\\HERE'],['/repo/','/repo'],['/repo','/repo2'],["/repo/a'b","/repo/a'b"]]) list({cwd:query,platformCase:recorded==='C:\\proj\\here' && query==='c:\\proj\\HERE',files:[{path:'p',cwd:recorded,msgs:[{text:'path opener'}]}]});
function summary(files, dirs=[], links=[]) {
 const home=join(root,String(cases.length)), dir=join(home,'memories','rollout_summaries'); mkdirSync(dir,{recursive:true});
 for(const [name, content] of Object.entries(files)) writeFileSync(join(dir,name),content);
 for(const name of dirs) mkdirSync(join(dir,name));
 for(const name of links) symlinkSync('absent-target',join(dir,name));
 cases.push({kind:'summary',in:{files,dirs,links},out:Object.fromEntries(cwd.loadSummaryIndex(home))});
}
summary({});
summary({'z.md':'thread_id: duplicate\n# Z wins\n','a.md':'thread_id: duplicate\n# A\n','bare.md':'# no id\n','headless.md':'thread_id: absent\n','ignore.txt':'thread_id: no\n# no\n'},['dir.md'],['broken.md']);
for(const sep of ['\n','\r','\r\n','\u2028','\u2029']) for(const heading of ['# first', '# ', '#  ', '#\nnext line', '# \u0085heading', '#\ufeffheading', '## not h1']) summary({'s.md':'thread_id:\n token'+sep+heading+sep+'# second'});
for(const content of ['thread_id: a extra\n# invalid id\n','thread_id: first\nthread_id: second\n# first heading\n# later heading\n',' '.repeat(1200)+'thread_id: late\n# late\n','thread_id: a\n'+'x'.repeat(1200)+'\n# too late\n','thread_id: a\n# '+'x'.repeat(1190)+'😀', 'thread_id: a\n# '+'x'.repeat(1184)+'😀']) summary({'s.md':content});
const sql = [
 "INSERT INTO files(path,mtime_ms,size,cwd,source,date) VALUES(NULL,0,0,'/repo','main','2026-01-02')",
 "INSERT INTO files(path,mtime_ms,size,cwd,source,date) VALUES('blob',0,0,'/repo','main',x'4344')",
 "INSERT INTO files(path,mtime_ms,size,cwd,source,date) VALUES(x'6566',0,0,'/repo','main','2026-01-02')",
 "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES('null',0,'ts','user','content',0,x'4142')",
 "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES('blob',0,'ts','user','content',0,x'0100410042')",
 "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES('101,102',0,'ts','user','content',0,'blob path message')",
];
const sqlHome=join(root,'sql-strings'); mkdirSync(sqlHome);
const sqlPath=join(sqlHome,'index.sqlite'), sqlDb=index.openIndex(sqlPath);
for(const statement of sql) sqlDb.exec(statement); sqlDb.close();
cases.push({kind:'sqliteStrings',in:{sql},out:cwd.listCwdSessions('/repo',5,{indexPath:sqlPath,home:sqlHome,readOriginUrl:()=>null})});
process.stdout.write(JSON.stringify(cases,null,2)+'\n');
