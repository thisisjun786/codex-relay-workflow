// CXC v0.2.40 (3c1459ac) rollout oracle; Node is used only to record synthetic answers.
// node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist <scratch>
import { mkdtempSync, mkdirSync, writeFileSync, symlinkSync, rmSync } from 'node:fs';
import { join } from 'node:path';
const r = await import(process.argv[2] + '/rollout.js');
const root = mkdtempSync(join(process.argv[3], 'rollout-'));
process.env.CODEX_HOME = root;
const cases = [];
const normalize = v => JSON.parse(JSON.stringify(v).split(root).join('$R'));
const add = (fn, args, call) => {
  try { cases.push({ fn, in: normalize(args), out: normalize(call()) }); }
  catch (e) { cases.push({ fn, in: normalize(args), error: fn === 'meta' || fn === 'list' ? 'io' : e.message }); }
};
const prefixes = ['<environment_context>', '<ENVIRONMENT_CONTEXT>', '<skill>', '<subagent_notification>', '<turn_aborted>', '<permissions instructions>', '<INSTRUCTIONS>', '<user_instructions>', '<system-reminder>', '# AGENTS.md instructions', '## Workspace Context', '[Recent Context]'];
for (const prefix of [...prefixes, '<SKILL>', 'human', '', '\u0085<skill>']) {
  for (const lead of ['', ' \t\ufeff']) add('synthetic', [lead + prefix], () => r.isSyntheticUserText(lead + prefix));
}
const paths = ['', '/', '///', '/repo/', '/repo2', '\\repo\\x', 'c:\\Repo\\', '\\\\?\\C:\\Repo\\', '//?/c:/Repo///', '//?/UNC/server/share/', '//?/unc/server/share/', '//?/UnC/server/share/', '//server/share', '//?/', '//?/UNC/', 'relative//x', 'İ/ΑΣ', '\u0085/repo', '😀', '\ue000'];
for (const path of paths) add('cwd', [path], () => r.normalizeCwd(path));
for (const a of paths) for (const b of paths) for (const fold of [false, true]) add('cwdMatches', [a,b,fold], () => r.cwdMatches(a,b,{ caseInsensitive:fold }));
for (const platform of ['linux','darwin','win32','windows','']) add('fold', [platform], () => r.foldCwdCaseFor(platform));
for (const name of ['rollout-2026-08-21T00-00-x.jsonl','rollout-0000-99-99T','rollout-2026-08-21','xrollout-2026-08-21T','rollout-２０２６-08-21T','rollout-2026-08-21T\n']) add('dateName',[name],()=>r.dateFromRolloutName(name));
add('sql',['cwd'],()=>r.canonicalCwdSql('cwd'));
for(const iso of ['2026-08-21T00:00:00Z','0001-01-02T03:04:05Z','2026-08-21T23:59:59-09:00']) add('localDate',[iso],()=>r.localDateString(new Date(iso)));
const response = p => JSON.stringify({ type:'response_item', timestamp:'t', payload:p });
const contents = [undefined,null,'hello',[],[{type:'input_text',text:' \ufeffhello\r\nworld\ufeff '}], [{type:'input_text',text:42},{type:'output_text',text:true},{type:'image',text:'omit'}], [null,{}, {type:'input_text',text:[1,null,[2,3],{}]}], [{type:'input_text',text:{toString:null}}], [{type:'input_text',text:1e21}], [{type:'output_text',text:1e-7}]];
const docs = ['','junk\n{"type":"session_meta"}\n','{"type":"response_item", bad}\n'];
for(const role of [undefined,'user','assistant','developer','unknown']) for(const content of contents) docs.push(response({type:'message',role,content}));
for(const p of prefixes) docs.push(response({type:'message',role:'user',content:[{type:'input_text',text:' \t'+p+' injected'}]}));
for(const output of [null,42,'  text  ',['x',{text:'y'},null],{text:'z'},{content:'c',text:'t'},{content:[{text:42},'b']},[{text:{}}],[{text:{toString:null}}],{content:3,text:'fallback'},[]]) docs.push(response({type:'function_call_output',output}));
for(const name of [undefined,null,'','run',{},[],{toString:null}]) for(const args of [undefined,'','x',42,[1,null],{}]) docs.push(response({type:'function_call',name,arguments:args}));
docs.push(response({type:'message',role:'user',content:[{type:'input_text',text:'CI'}]}).replace('CI','\\u0043I'));
docs.push(response({type:'message',content:[{type:'input_text',text:'skip'}]}).replace('response_item','response_\\u0069tem'));
docs.push('{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":1e400}]}}');
docs.push([response({type:'message',role:'user',content:[{type:'input_text',text:'first'}]}),response({type:'message',role:'assistant',content:[{type:'output_text',text:'second'}]}),''].join('\n'));
for(const doc of docs) for(const tools of [false,true]) { add('parse',[doc,tools],()=>r.parseRollout(doc,tools)); if(doc.includes('\n'))add('parse',[doc.replace(/\n/g,'\r\n'),tools],()=>r.parseRollout(doc.replace(/\n/g,'\r\n'),tools)); }
const plan = {required:[[{text:'ci',boundary:false}]],optional:[],minOptional:0,anyMode:false};
for(const content of ['','ci','precision',docs.at(-4).toLowerCase()]) add('prefilter',[content,plan],()=>r.matchesFilePrefilter(content,plan));
add('prefilter',['ci',{required:[],optional:[],minOptional:0,anyMode:false}],()=>r.matchesFilePrefilter('ci',{required:[],optional:[],minOptional:0,anyMode:false}));
for(const payload of [{},{id:'',cwd:'',agent_nickname:'',originator:''},{id:'id',cwd:'/repo',thread_source:'subagent',git:{repository_url:'git@example.test:group/repo.git'}},{source:{subagent:null}},{source:{subagent:false}},{source:{}},{id:42,cwd:[],git:{repository_url:42}},{git:{repository_url:'https://example.test/group/repo.git'}},null,42,[]]) {
  const head=JSON.stringify({type:'session_meta',payload});add('meta',[head],()=>{writeFileSync(join(root,'meta.jsonl'),head+'\n'+response({type:'message',content:[]}));return r.readRolloutMeta(join(root,'meta.jsonl'));});
}
for(const head of ['', 'bad', '{"type":"message"}', JSON.stringify({type:'session_meta',payload:{id:'big',instructions:'x'.repeat(44000)}}), JSON.stringify({type:'session_meta',payload:{id:'too-big',instructions:'x'.repeat(1048576)}})]) add('meta',[head],()=>{writeFileSync(join(root,'meta.jsonl'),head);return r.readRolloutMeta(join(root,'meta.jsonl'));});
add('meta',[null],()=>r.readRolloutMeta(join(root,'missing')));
const files=['sessions/2026/08/21/a.jsonl','sessions/2026/08/21/z.jsonl','sessions/2026/08/20/old.jsonl','sessions/2026/08/21/ignored.txt','sessions/2026/08/22/new.jsonl','sessions/2026/99/00/strange.jsonl','archived_sessions/rollout-2026-08-21T12-00-x.jsonl','archived_sessions/rollout-2026-08-20T12-00-x.jsonl','archived_sessions/unparseable.jsonl','archived_sessions/rollout-2026-99-99T.jsonl'];
for(const file of files){mkdirSync(join(root,file,'..'),{recursive:true});writeFileSync(join(root,file),'');}
mkdirSync(join(root,'sessions/2026/08/21/directory.jsonl'));
mkdirSync(join(root,'sessions/2026/08/21/😀.jsonl'));
mkdirSync(join(root,'sessions/2026/08/21/\ue000.jsonl'));
symlinkSync(join(root,'sessions/2026'),join(root,'sessions/link'));
Date.now=()=>Date.parse('2026-08-22T00:00:00Z');
for(const days of [0,-1,1,0.5,2,100,1e20]) add('list',[days],()=>r.listRolloutFiles(root,days));
rmSync(root,{recursive:true,force:true});
if (process.argv[4] === '--platform-limits') {
  const first = response({type:'message',content:[{type:'input_text',text:'first'}]});
  const deep = '{"type":"response_item","timestamp":"t","payload":{"type":"message","content":[{"type":"input_text","text":' + '['.repeat(8000) + '1' + ']'.repeat(8000) + '}]}}';
  const limits = [false,true].map(prepend => {
    const c = {depth:8000,prepend,classification:'intentionally-changed',reason:'V8 stack exhaustion is engine-dependent; Go coerces these valid nested arrays and returns preceding entries. No runtime Node or arbitrary emulated stack cap.',goExpectedTexts:prepend?['first','1']:['1']};
    try { c.oracleEntries = r.parseRollout(prepend?first+'\n'+deep:deep,false); }
    catch (e) { c.oracleError = e.message; c.oracleErrorType = e.name; }
    return c;
  });
  process.stdout.write(JSON.stringify(limits)+'\n');
} else {
process.stdout.write(JSON.stringify(cases)+'\n');
}
