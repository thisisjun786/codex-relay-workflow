import {readFileSync,writeFileSync} from 'node:fs';
import {pathToFileURL} from 'node:url';
const [oracle, repo, output] = process.argv.slice(2);
if (!oracle || !repo || !output) throw Error('usage: node record-directives.mjs ORACLE REPO OUTPUT');
const table=JSON.parse(readFileSync(repo+'/contract/schema/cxc/name-substitution.json','utf8'));
const rename = raw => {
 let s=raw;
 for(const rule of table.rules) {
  if(rule.kind==='rewrite')continue;
  const re=new RegExp(rule.regex,'g');
  const replacement=rule.replace.replace(/\$\{(\d+)\}/g,(_,n)=>'$'+n);
  for(;;){const next=s.replace(re,replacement);if(next===s||!rule.repeat){s=next;break;}s=next;}
  if(rule.kind==='resolver'){
   const rows=[...table.cli].sort((a,b)=>b.cxc.length-a.cxc.length);
   const escape=s=>s.replace(/[.*+?^${}()|[\]\\]/g,'\\$&');
   const verbs=new RegExp('(\\{CRW\\}|\\bcxc) ('+rows.map(r=>escape(r.cxc.join(' '))).join('|')+')\\b','g');
   s=s.replace(verbs, (match,prefix,verb)=>{const row=rows.find(r=>r.cxc.join(' ')===verb);return row.crw?prefix+' '+row.crw.join(' '):match;});
  }
 }
 return s;
};
process.env.CODEXCLAW_CXC='node "${PLUGIN_ROOT}/bin/cxc.mjs"';
const hook=await import(pathToFileURL(oracle+'/plugins/codexclaw/components/pabcd-state/dist/hook.js'));
const minds=await import(pathToFileURL(oracle+'/plugins/codexclaw/components/pabcd-state/dist/minds.js'));
const result={oracle:'CXC v0.2.40 commit 3c1459ac',constants:{},phases:[],platforms:[],resolutions:[]};
for(const key of ['QUESTION_SHAPE_DIRECTIVE','AGBROWSE_SEARCH_DIRECTIVE','TRIGGER_AUTHORITY_NOTE'])result.constants[key]=rename(hook[key]);
result.constants.MIND_DISPATCH_DIRECTIVE=rename(minds.MIND_DISPATCH_DIRECTIVE);
const src=readFileSync(oracle+'/plugins/codexclaw/components/pabcd-state/dist/hook.js','utf8');
result.constants.PA_ATTEST_EXAMPLE=JSON.parse(src.match(/const PA_ATTEST_EXAMPLE\s*=\s*'([^']+)'/)[1]);
// Store the attest as text, preserving JSON.stringify's exact compact spelling.
result.constants.PA_ATTEST_EXAMPLE=JSON.stringify(result.constants.PA_ATTEST_EXAMPLE);
for(const phase of ['IDLE','I','P','A','B','C','D','FUTURE']) {
 const opts={activeWorkPhase:{id:'wp2',title:'한국어 🧪 slice'}};
 result.phases.push({phase,directive:rename(hook.phaseDirective(phase)),active:rename(hook.phaseDirective(phase,opts)),header:rename(hook.buildStageHeader(phase)),footer:rename(hook.phaseFooter(phase)),withFooter:rename(hook.withFooter('sample\r\n',phase)),empty:hook.withFooter('',phase)});
}
for(const platform of ['linux','darwin','win32','windows','freebsd','unknown']) result.platforms.push({platform,directive:rename(hook.loopArmDirective(platform))});
result.interview=rename(hook.interviewDirective());
result.options=[null,{}, {activeWorkPhase:null},{activeWorkPhase:{id:'',title:''}},{activeWorkPhase:{id:' wp3 ',title:' 🧪 한글\r\n '}}].map(opts=>({opts,output:rename(hook.phaseDirective('B',opts))}));
result.footers=['',' ','\r\n','already\nfooter'].map(input=>({input,output:rename(hook.withFooter(input,'P'))}));
const override=process.env.CODEXCLAW_CXC;process.env.CODEXCLAW_CXC=' literal $& $$ $1 ';
result.literal={input:rename('`cxc scan record`'),output:rename(hook.resolveCxcInDirective('`cxc scan record`').split('literal $& $$ $1').join(override)).split('{CRW}').join('literal $& $$ $1')};process.env.CODEXCLAW_CXC=override;
for(const input of ['`cxc orchestrate P`','owns cxc orchestration; cxc-loop; `cxc scan record`','`cxc ` `cxc\torchestrate` `cxc \u00a0scan`','`cxc foo\u0085bar` `cxc foo\ufeffbar`','`cxc foo` and `cxc loop show`','`cxc open-end','`cxc \u0085scan` `cxc \ufeffscan`']) result.resolutions.push({input:rename(input),output:rename(hook.resolveCxcInDirective(input))});
const savedEnv=process.env;let calls=0;
const failureInput='`cxc scan record` and `cxc loop show`';
process.env=new Proxy(savedEnv,{get(target,key){if(key==='CODEXCLAW_CXC'&&++calls===2)throw Error('recorded invocation failure');return target[key];}});
try{result.failOpen={input:rename(failureInput),output:rename(hook.resolveCxcInDirective(failureInput)),calls};}finally{process.env=savedEnv;}
writeFileSync(output,JSON.stringify(result,null,2)+'\n');
console.log(JSON.stringify({phases:result.phases.length,platforms:result.platforms.length,constants:Object.keys(result.constants).length,resolutions:result.resolutions.length}));
