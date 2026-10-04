// CXC v0.2.40 (3c1459ac), orchestrate-cli.ts:301-340,389-418,466-548,1139-1160.
// node record-oracle.mjs <extracted oracle root> <output directory>
// All writes, HOME and SQLite files stay in the output directory; oracle is read-only.
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
import {DatabaseSync} from 'node:sqlite';
const [oracle, out] = process.argv.slice(2);
const O = await import(pathToFileURL(path.resolve(oracle, 'plugins/codexclaw/components/pabcd-state/dist/orchestrate-cli.js')));
fs.mkdirSync(out, {recursive:true});
const state = phase => JSON.stringify({phase, flags:{auditPassed:true,checkPassed:false}});
const files = {'ws/.codexclaw/sessions/s.json':state('P')};
const id = '11111111-1111-4111-8111-111111111111';
const native = {CODEX_THREAD_ID:id, CODEX_HOME:'$R/codex'};
const sqlite = ['CREATE TABLE threads (id TEXT PRIMARY KEY, cwd TEXT, archived INTEGER, source TEXT)', `INSERT INTO threads VALUES ('${id}', '$R/ws', 0, 'vscode')`];
const cases = [
  {id:'empty_text',argv:['status']},
  {id:'empty_json_is_text',argv:['status','--json']},
  ...['help','-h','--help'].map(token=>({id:'help_'+token.replaceAll('-','dash'),argv:[token]})),
  {id:'status_help',argv:['status','--help']},
  {id:'missing_json',argv:['status','--session','missing','--json']},
  {id:'missing_text',argv:['status','--session','missing']},
  {id:'explicit_text',argv:['status','--session','s'],files},
  {id:'explicit_json',argv:['status','--session','s','--json'],files},
  {id:'latest_text',argv:['status'],files},
  {id:'latest',argv:['status','--json'],files:{'ws/.codexclaw/sessions/old.json':state('A'),'ws/.codexclaw/sessions/new.json':state('B'),'ws/.codexclaw/sessions/ignored.json.tmp':'{}'},mtimes:{'ws/.codexclaw/sessions/old.json':100,'ws/.codexclaw/sessions/new.json':200,'ws/.codexclaw/sessions/ignored.json.tmp':300}},
  {id:'tie',argv:['status','--json'],files:{'ws/.codexclaw/sessions/z.json':state('A'),'ws/.codexclaw/sessions/a.json':state('C')},mtimes:{'ws/.codexclaw/sessions/z.json':200,'ws/.codexclaw/sessions/a.json':200}},
  {id:'corrupt_idle',argv:['status','--session','s','--json'],files:{'ws/.codexclaw/sessions/s.json':'garbage'}},
  {id:'directory_idle',argv:['status','--session','dir','--json'],dirs:['ws/.codexclaw/sessions/dir.json']},
  {id:'raw_and_sanitized',argv:['status','--session','raw id','--json'],files:{'ws/.codexclaw/sessions/raw id.json':state('B'),'ws/.codexclaw/sessions/raw-id.json':state('A')}},
  {id:'unicode_json',argv:['status','--session','<&>\u2028\u2029\\u2028','--json'],files:{['ws/.codexclaw/sessions/<&>\u2028\u2029\\u2028.json']:state('B')}},
  {id:'attest_bad_existing',argv:['A','--session','s','--attest','{bad'],files},
  {id:'attest_bad_missing',argv:['A','--session','missing','--attest','{bad']},
  {id:'attest_shape_existing',argv:['A','--session','s','--attest','{}'],files},
  {id:'attest_shape_missing',argv:['D','--session','missing','--attest','{}']},
  {id:'attest_shape_illegal',argv:['D','--session','s','--attest','{}'],files},
  {id:'attest_both',argv:['A','--session','s','--attest','{}','--attest-file','a.json'],files:{...files,'ws/a.json':'{}'}},
  {id:'status_ignores_attest',argv:['status','--session','s','--attest','{bad','--json'],files},
  {id:'mutation_no_session',argv:['P'],files},
  {id:'mutation_empty_session',argv:['P','--session',''],files},
  {id:'mutation_unknown_session',argv:['P','--session','ghost'],files},
  {id:'parse_existing',argv:['wat','--session','s'],files},
  {id:'parse_missing',argv:['wat','--session','ghost'],files},
  {id:'foreign_text',argv:['status','--session','s'],files:{...files,'home/foreign/.codexclaw/sessions/s.json':state('B')}},
  {id:'foreign_json',argv:['status','--session','s','--json'],files:{...files,'home/foreign/.codexclaw/sessions/s.json':state('B')}},
  ...['','not-a-uuid','../parent'].map((value,i)=>({id:'native_invalid_'+i,argv:['status','--json'],files,native:{CODEX_THREAD_ID:value,CODEX_HOME:'$R/codex'}})),
  {id:'native_db_missing',argv:['status','--json'],files,native},
  {id:'native_invalid_text',argv:['status'],files,native:{CODEX_THREAD_ID:'invalid'}},
  {id:'native_empty_explicit',argv:['status','--session','','--json'],files,native,sqlite},
  {id:'native_selects_root',argv:['status','--json'],files:{[`ws/.codexclaw/sessions/${id}.json`]:state('P'),'ws/.codexclaw/sessions/parent.json':state('B')},mtimes:{[`ws/.codexclaw/sessions/${id}.json`]:100,'ws/.codexclaw/sessions/parent.json':200},native,sqlite},
  {id:'native_missing_no_fallback',argv:['status','--json'],files,native,sqlite},
  {id:'explicit_ignores_native',argv:['status','--session','s','--json'],files,native:{CODEX_THREAD_ID:'invalid'}},
  {id:'newest_schema_unsupported',argv:['status','--json'],files,native,sqlite,extraDB:true},
];
for (const c of cases) {
  const root = path.resolve(out, 'world', c.id);
  const expand = s => s.replaceAll('$R',root);
  for(const dir of ['ws','home','codex','crw',...(c.dirs||[])])fs.mkdirSync(path.join(root,dir),{recursive:true});
  process.env.HOME=path.join(root,'home');process.env.CODEX_HOME=path.join(root,'codex');process.env.CODEXCLAW_HOME=path.join(root,'crw');process.env.CRW_HOME=path.join(root,'crw');
  for(const [name,contents] of Object.entries(c.files||{})){const p=path.join(root,name);fs.mkdirSync(path.dirname(p),{recursive:true});fs.writeFileSync(p,expand(contents));}
  for(const [name,sec] of Object.entries(c.mtimes||{}))fs.utimesSync(path.join(root,name),sec,sec);
  if(c.sqlite){const db=new DatabaseSync(path.join(root,'codex/state_5.sqlite'));for(const stmt of c.sqlite)db.exec(expand(stmt));db.close();}
  if(c.extraDB){const db=new DatabaseSync(path.join(root,'codex/state_6.sqlite'));db.exec('CREATE TABLE threads (id TEXT)');db.close();}
  const args=O.parseOrchestrateCliArgs(c.argv.map(expand),path.join(root,'ws'));
  c.want='error' in args?{code:1,output:O.renderOrchestrateParseError(args)}:O.runOrchestrateCli(args,{},Object.fromEntries(Object.entries(c.native||{}).map(([k,v])=>[k,expand(v)])));
  c.want.output=c.want.output.replaceAll(root,'$R');
}
fs.writeFileSync(path.join(out,'oracle.json'),JSON.stringify({oracle:'CXC v0.2.40 3c1459ac',cases},null,2)+'\n');
console.log(`recorded ${cases.length} session/status answers`);
