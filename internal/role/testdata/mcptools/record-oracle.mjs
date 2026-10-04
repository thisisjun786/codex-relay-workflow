// Run with Node 24, an extracted v0.2.40 tree and an owned scratch directory.
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
const [oracle, scratch, output] = process.argv.slice(2);
fs.mkdirSync(scratch, { recursive: true });
const base = path.join(oracle, 'plugins/codexclaw/components/subagent-config/src');
const src = fs.readFileSync(path.join(base, 'mcp.ts'), 'utf8').replace(/from "(\.\/[^"\n]+)"/g, (_, p) => `from "${pathToFileURL(path.resolve(base, p)).href}"`);
const copy = path.join(scratch, 'oracle-mcp.ts');
fs.writeFileSync(copy, src + '\nexport { TOOLS, decorateSubagentsGet, catalogIsAuthoritative };\n');
const mod = await import(pathToFileURL(copy));
const at = new Date('2026-01-01T12:00:00.000Z');
const fresh = (source = 'ocx', status = 'fresh') => ({ state: source === 'ocx' ? 'ocx-active' : 'native-catalog', entries: [{id:'provider/present',source,label:'provider/present'}], status, source, fetchedAt: at.toISOString() });
const cases = [];
const get = {name:'subagents_get',arguments:{scope:'global'}};
const set = (role, patch) => ({name:'subagents_set',arguments:{scope:'global',role,...patch}});
const record = async (id, ops, catalog = fresh(), nativeAge = null, initialStore = null) => {
  const dir = path.join(scratch, id);
  for (const d of ['home','codex','crw']) fs.mkdirSync(path.join(dir,d),{recursive:true});
  Object.assign(process.env,{HOME:path.join(dir,'home'),CODEX_HOME:path.join(dir,'codex'),CODEXCLAW_HOME:path.join(dir,'crw')});
  delete process.env.CODEX_MODELS_CACHE_PATH;
  if (nativeAge !== null) {
    process.env.CODEX_MODELS_CACHE_PATH = path.join(dir,'codex','models.json');
    fs.writeFileSync(process.env.CODEX_MODELS_CACHE_PATH,'{"models":[]}');
    const modified = new Date(Date.now()-nativeAge);
    fs.utimesSync(process.env.CODEX_MODELS_CACHE_PATH, modified, modified);
  }
  const file = path.join(dir,'crw','subagents.json');
  if (initialStore !== null) fs.writeFileSync(file,initialStore);
  const answers = [];
  for (const params of ops) {
    const write = process.stdout.write;
    let text = '';
    process.stdout.write = chunk => { text += chunk.toString();return true; };
    let error = null;
    try { await mod.handleToolCall(1,params,async () => {if(catalog === 'reject')throw Error('catalog failed');return catalog;}); }
    catch(e) {error=e.message;}
    finally {process.stdout.write=write;}
    answers.push({params,result:text ? JSON.parse(text).result : null,error,store:fs.existsSync(file)?fs.readFileSync(file,'utf8'):null});
  }
  cases.push({id,catalog,nativeAge,initialStore,answers});
};
await record('role_roundtrip',[set('reviewer',{mode:'model',model:'provider/present',effort:'low',promptOverride:'<prompt>\u2028'}),get]);
await record('four_roles_fallback', ['explorer','reviewer','executor','architect'].flatMap(role=>[set(role,{fallback:{model:'backup/model',effort:'low'}}),get]));
await record('architect_independent',[set('architect',{mode:'model',model:'design-fixture',effort:'high'}),get]);
await record('invalid_settings', [set('reviewer',{mode:'turbo'}),set('executor',{effort:'turbo'}),set('executor',{fallback:{effort:'invalid'}}),set('nobody',{mode:'default'}),set('explorer',{inherit:true,mode:'default'}),set('explorer',{inherit:'yes'}),{name:'subagents_get',arguments:{scope:'galaxy'}}]);
await record('inherit_reset',[set('explorer',{mode:'model',model:'provider/present',effort:'xhigh'}),set('explorer',{inherit:true}),get]);
await record('decorated_roles',[set('explorer',{mode:'model',model:'provider/present',effort:'low'}),set('reviewer',{mode:'model',model:'provider/missing'}),set('architect',{effort:'high'}),get]);
for(const status of ['stale','unavailable'])await record('catalog_'+status,[set('reviewer',{mode:'model',model:'provider/missing'}),get],fresh('ocx',status));
await record('catalog_rejected',[set('reviewer',{mode:'model',model:'provider/missing'}),get],'reject');
await record('native_fresh',[set('reviewer',{mode:'model',model:'provider/missing'}),get],fresh('native'),0);
await record('native_old',[set('reviewer',{mode:'model',model:'provider/missing'}),get],fresh('native'),25*60*60*1000);
await record('argument_shapes',[{name:'subagents_set'},...['subagents_set','subagents_get'].flatMap(name=>[null,5,'x',[],true].map(arguments_=>({name,arguments:arguments_})))]);
await record('unknown_names',[{},...[null,3,[],{toString:1}].map(name=>({name})),{name:'unknown'}]);
const timeoutDir=path.join(scratch,'timeout');fs.mkdirSync(timeoutDir,{recursive:true});
const store=await import(pathToFileURL(path.join(base,'store.ts')));
store.setRole(scratch,'reviewer',{mode:'model',model:'provider/model'},'global');
const settings=store.readSettings(scratch,'global');
const timeout=mod.decorateSubagentsGet(settings,null,at.getTime(),process.env);
fs.writeFileSync(output,JSON.stringify({oracle:'CXC v0.2.40 3c1459ac',tools:mod.TOOLS,cases,timeout},null,2)+'\n');
console.log(`Recorded ${cases.length} library cases.`);
