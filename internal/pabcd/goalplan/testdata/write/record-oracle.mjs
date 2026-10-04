// Usage: node record-oracle.mjs <read-only extracted CXC v0.2.40 tree>
import fs from 'node:fs';
import {join, resolve} from 'node:path';
import {tmpdir} from 'node:os';
import {pathToFileURL} from 'node:url';
import {syncBuiltinESMExports} from 'node:module';
const oracle=await import(pathToFileURL(join(resolve(process.argv[2]),'plugins/codexclaw/components/pabcd-state/dist/goalplan.js')).href);
const root=fs.mkdtempSync(join(tmpdir(),'goalplan-write-oracle-'));
const now=()=> '2026-01-01T00:00:00.000Z';
const builds=[];
for(const [id,schema] of [['default',undefined],['zero',0],['negative',-7],['nan',NaN],['inf',Infinity],['negative-inf',-Infinity],['two',2],['fraction',2.9],['three',3],['future',99]]) builds.push({id,oracle:oracle.buildGoalplan({objective:'Hello, World!!',schemaVersion:schema,now})});
builds.push({id:'criteria-host',oracle:oracle.buildGoalplan({objective:'Ship & <export> \u2028\\u2028',criteria:[{scenario:'CSV export works'},{scenario:'Native shell',expectedEvidence:'screen',surface:'desktop',presented:'native'},{scenario:'empty surface',surface:''}],host:{armed:true,armedAt:'2025-12-31T00:00:00.000Z',source:'freeze'},now})});
const cwd=join(root,'ledger');fs.mkdirSync(cwd);const dir=oracle.goalplanDir(cwd,'led');fs.mkdirSync(dir,{recursive:true});const first={ts:'t1',slug:'led',event:'created',detail:'a'};const second={ts:'t2',slug:'led',event:'task_done',detail:'b'};fs.writeFileSync(join(dir,'ledger.jsonl'),JSON.stringify(first));oracle.appendGoalplanLedger(cwd,'led',second);const unterminated=fs.readFileSync(join(dir,'ledger.jsonl'),'utf8');
const security=[];
for(const mode of ['write','append']){
 const ws=join(root,mode),outside=join(root,mode+'-outside');fs.mkdirSync(ws);fs.mkdirSync(join(outside,'safe'),{recursive:true});fs.mkdirSync(join(ws,'.codexclaw','goalplans','safe'),{recursive:true});const plans=join(ws,'.codexclaw','goalplans');
 const orig=mode==='write'?fs.writeFileSync:fs.openSync;let swapped=false;
 const replacement=function(path,...args){if(!swapped && typeof path==='string' && path.startsWith(plans+'/safe/') && (mode==='append'?path.endsWith('ledger.jsonl'):path.endsWith('.tmp'))){swapped=true;fs.renameSync(plans,plans+'-old');fs.symlinkSync(outside,plans);}return orig(path,...args);};
 if(mode==='write')fs.writeFileSync=replacement;else fs.openSync=replacement;syncBuiltinESMExports();let accepted=true,error='';
 try{if(mode==='write'){const p=oracle.buildGoalplan({objective:'safe',now});oracle.writeGoalplan(ws,p);}else oracle.appendGoalplanLedger(ws,'safe',{ts:'t',slug:'safe',event:'created',detail:'x'});}catch(e){accepted=false;error=e.code??e.name;}
 if(mode==='write')fs.writeFileSync=orig;else fs.openSync=orig;syncBuiltinESMExports();security.push({id:'after-check-'+mode,accepted,error,outside:fs.readdirSync(outside,{recursive:true}),classification:'intentionally-changed',reason:'Security: path replacement after the check directs the oracle write outside the workspace; descriptor-bound no-follow writes refuse it.'});
}
const output={oracle:'CXC v0.2.40, commit 3c1459ac',builds,unterminated:{first,second,oracle:unterminated,classification:'intentionally-changed',reason:'The appended record fuses with the unterminated final record; both become invalid JSONL and are lost.'},security};process.stdout.write(JSON.stringify(output,null,2)+'\n');fs.rmSync(root,{recursive:true,force:true});
