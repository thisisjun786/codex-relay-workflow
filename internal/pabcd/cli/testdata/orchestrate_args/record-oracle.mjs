// CXC v0.2.40 (3c1459ac), orchestrate-cli.ts:169-300,357-388.
// Usage: node record-oracle.mjs <oracle dist file URL> <output directory>
// Writes only the output directory. The oracle trees remain read-only.
import {mkdirSync, writeFileSync} from 'node:fs';
import {resolve} from 'node:path';
const [dist,out] = process.argv.slice(2);
const O = await import(dist+'/orchestrate-cli.js');
mkdirSync(out,{recursive:true});
const cwd=resolve(out,'world'); mkdirSync(cwd,{recursive:true});
const good='{"from":"P","to":"A","did":" x y ","auditVerdict":" PASS ","exitCode":1e999}';
const cases=[['CONSTRUCTOR'],['__PROTO__'],['a','--attest','{"from":"P","from":"C","to":"D","did":"𝒜"}'],['a','--attest','{"from":"P","to":"A","exitCode":-0}'],[''],['İ'],['K'],['A','--cwd','ignored','--help'],['a','--session','one','--session','two','--cwd','first','--cwd','second'],[],['status'],['a','--attest',good],['idle'],['wat','--session','first','--session','last','--cwd','one','--cwd','two'],['--help'],['help'],['-h'],['status','--session','help'],['A','unknown','--other','x','--json'],['--session','s','status'],[' status '],['STATUS'],['constructor'],['__proto__'],['toString'],['a','--session',''],['a','--session'],['a','--cwd'],['a','--cwd',''],['a','--session','--json'],['a','--cwd','--json'],['a','--session=x','--cwd=y','--json=true'],['a','--attest'],['a','--attest',''],['a','--attest','{}'],['a','--attest','[]'],['a','--attest','null'],['a','--attest','{"from":1,"to":"A"}'],['a','--attest','{"from":"bad","to":"worse"}'],['a','--attest','{}','--attest',good],['a','--attest',good,'--attest','bad'],['a','--attest',good,'--attest',good],['a','--attest','{"from":"P","to":"A"} trailing'],['a','--attest','\uFEFF'+good],['a','--attest-file'],['a','--attest',good,'--attest-file','f'],['a','--attest-file','f','--attest',good],['a','--attest',good,'--attest-file']];
const normalize=r=>{if(typeof r.verb==='function')r.verb='constructor'; else if(r.verb&&typeof r.verb==='object')r.verb='__proto__';return r;};
const parser=cases.map((argv,i)=>({id:'argv_'+i,argv,cwd:'/ws',want:normalize(O.parseOrchestrateCliArgs(argv,'/ws'))}));
const fileCases=[
{ id:'file_trailing_data',files:{'att.json':good+' trailing'},argv:['a','--attest-file','att.json','--cwd','$R']},
{ id:'file_empty_path',files:{},argv:['a','--attest-file','','--cwd','$R']},
{ id:'file_valid_late_cwd',files:{'att.json':good},argv:['a','--session','s1','--attest-file','att.json','--cwd','$R']},
{ id:'file_bom',files:{'att.json':'\uFEFF'+good},argv:['a','--attest-file','att.json','--cwd','$R']},
{ id:'file_crlf',files:{'att.json':'{\r\n"from":"P",\r\n"to":"A",\r\n"did":"crlf attest"\r\n}\r\n'},argv:['a','--attest-file','att.json','--cwd','$R']},
{ id:'file_missing',files:{},argv:['a','--attest-file','missing.json','--cwd','$R']},
{ id:'file_invalid_json',files:{'att.json':'{nope}'},argv:['a','--attest-file','att.json','--cwd','$R']},
{ id:'file_shape',files:{'att.json':'{"did":"x"}'},argv:['a','--attest-file','att.json','--cwd','$R']},
{ id:'file_both',files:{'att.json':good},argv:['a','--attest-file','att.json','--attest',good,'--cwd','$R']},
{ id:'file_last_wins',files:{'att.json':good},argv:['a','--attest-file','missing.json','--attest-file','att.json','--cwd','$R']},
{ id:'file_error_sticks',files:{'att.json':good},argv:['a','--attest-file','att.json','--cwd','$R','--attest-file']},
{ id:'file_double_bom',files:{'att.json':'\uFEFF\uFEFF'+good},argv:['a','--attest-file','att.json','--cwd','$R']},
];
for (const c of fileCases){const root=resolve(cwd,c.id);mkdirSync(root,{recursive:true}); for(const [p,b] of Object.entries(c.files))writeFileSync(resolve(root,p),b);const r=normalize(O.parseOrchestrateCliArgs(c.argv.map(x=>x.replaceAll('$R',root)),'/unused'));c.want=JSON.parse(JSON.stringify(r).replaceAll(root,'$R'));}
const hints=[];for(const verb of ['I','P','A','B','C','D','status','reset','constructor','__proto__'])for(const from of [null,'','IDLE','I','P','A','B','C','D','Z'])hints.push({verb,from,want:O.renderAttestShapeHint(verb==='constructor'?Object:verb==='__proto__'?Object.prototype:verb,from)});
const fixture={oracle:'CXC v0.2.40 3c1459ac',parser,files:fileCases,hints,help:{linux:O.renderOrchestrateHelp('linux'),win32:O.renderOrchestrateHelp('win32')}};
writeFileSync(resolve(out,'oracle.json'),JSON.stringify(fixture,null,2)+'\n');
console.log(`recorded ${parser.length} argv, ${fileCases.length} file, ${hints.length} hint and 2 help answers`);
