// Node 24 recorder for CXC v0.2.40 3c1459ac recall/src/chat-search.ts.
// Usage: node record-oracle.mjs <oracle-root> <scratch-root> > oracle.json
// Exposes private functions in a verbatim scratch copy; import locations and
// appended exports are the only edits. Neither oracle source tree is modified.
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
const [root, scratch] = process.argv.slice(2);
const src = path.join(root, "plugins/codexclaw/components/recall/src");
fs.mkdirSync(scratch, { recursive: true });
const copy = path.join(scratch, "chat-search.ts");
fs.writeFileSync(copy, fs.readFileSync(path.join(src, "chat-search.ts"), "utf8").replace(/from "\.\//g, 'from "' + pathToFileURL(src + "/").href) + "\nexport { searchViaScan, contextWindow };\n");
const c = await import(pathToFileURL(copy));
const f = await import(pathToFileURL(path.join(root, "plugins/codexclaw/components/recall/test/fixtures.ts")));
const now = Date.parse("2026-10-04T12:00:00.000Z");
Date.now = () => now;
const cases = [];
const queries = ["", "trigram", "trigram korean", "aardwolf", "zebra", "zebra-in-tool-output", "ancient question", "트라이그램", "한글 트라이그램 결과", "CI", "the missing token"];
const options = [{}, {days:0,source:"all"}, {days:0,source:"all",any:true}, {days:0,source:"subagent"}, {days:0,role:"user",includeSynthetic:true}, {days:0,includeTools:false}, {days:0,context:1}, {days:0,limit:1}, {days:0,limit:2.5,context:.5}, {days:0,cwd:"/proj/beta"}, {days:0,cwd:"/different/alpha"}, {days:0,source:""}, {days:0,synonyms:true}, {days:7.5,order:"relevance",nowMs:0}];
const clean = (value, home) => JSON.parse(JSON.stringify(value).split(home).join("$R").replace(/unreadable rollout: ([^\n]*?) \([^\n]*?\)/g, "unreadable rollout: $1 (io)"));
const numeric = (opts, kind) => {
  if (kind === "nan-limit") opts.limit = NaN;
  if (kind === "nan-context") opts.context = NaN;
  if (kind === "nan-days") opts.days = NaN;
  if (kind === "infinite-days") opts.days = Infinity;
  if (kind === "infinite-context") opts.context = Infinity;
};
const run = (query, opts, kind = "base", files = {}) => {
  const home = fs.mkdtempSync(path.join(scratch, "home-"));
  if (kind !== "bare") f.buildCodexHome(home);
  if (kind === "nl") f.addNlGoldenCorpus(home);
  for (const [name, text] of Object.entries(files)) { const p = path.join(home, name); fs.mkdirSync(path.dirname(p), {recursive:true}); fs.writeFileSync(p, text); }
  if (kind === "directory") fs.mkdirSync(path.join(home, "sessions/2026/10/04/rollout-2026-10-04T99-directory.jsonl"));
  if (kind === "oversized") { const p = path.join(home, "sessions/2026/10/04/rollout-2026-10-04T99-large.jsonl"); fs.writeFileSync(p, '{"type":"session_meta","payload":{}}\n'); fs.truncateSync(p, 2**31); }
  const actual = {...opts}; numeric(actual, kind);
  const plan = c.chatMatchPlan(query, actual.any ?? false, actual.synonyms === true);
  const shared = {home, days:actual.days ?? c.DEFAULT_DAYS, limit:Math.min(Math.max(actual.limit ?? c.DEFAULT_LIMIT,1),c.MAX_LIMIT), contextN:Math.max(actual.context ?? 0,0), plan, source:actual.source ?? "main", repoKey:actual.cwd ? "github.com/example/alpha" : null};
  const row = {fn:"scan", query, opts, kind, files};
  try { const out = c.searchViaScan(query, actual, shared); out.elapsedMs=0; row.out=clean(out,home); }
  catch(e) { row.error = e.name === "TypeError" || e.name === "RangeError" ? e.message : "io"; }
  cases.push(row);
};
for (const query of queries) for (const opts of options) run(query, opts);
for (const kind of ["nan-limit","nan-context","nan-days","infinite-days","infinite-context","bare","directory","oversized"]) run(kind === "oversized" ? "needle" : "trigram", {days:0}, kind);
run("",{},"infinite-days");
for (const query of ["지난번 로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인한 방법","코덱스를 재시작하면 플러그인이 사라지는 문제","2.49.0 배포하고 npm 패키지가 진짜 그 소스인지 검증한 기록"]) run(query,{days:0,synonyms:true},"nl");
const message = (ts, text) => JSON.stringify({type:"response_item",timestamp:ts,payload:{type:"message",role:"user",content:[{type:"input_text",text}]}})+"\n";
const edgeName="sessions/2026/10/04/rollout-2026-10-04T99-edge.jsonl";
run("cutoff",{days:1},"base",{[edgeName]:message("2026-10-03T11:59:59.999Z","cutoff before")+message("2026-10-03T12:00:00.000Z","cutoff boundary")+message("","cutoff empty")});
run("chronology",{days:0,limit:1},"base",{[edgeName]:message("a","chronology first")+message("z","chronology newest")});
run("units",{days:0},"base",{[edgeName]:message("\u{10000}","units astral")+message("\ue000","units bmp")});
run("CI",{days:0},"base",{[edgeName]:message("","CI").replace('"CI"','"\\u0043I"')});
run("bomb",{days:0},"base",{[edgeName]:message("","bomb")+message("",{toString:null})});
for (const query of queries) for (const any of [false,true]) for (const synonyms of [false,true]) cases.push({fn:"plan",query,any,synonyms,out:c.chatMatchPlan(query,any,synonyms)});
const entries=[{ts:"a",role:"user",text:"one"},{ts:"b",role:"assistant",text:"two"}];
for (const [index,n] of [[0,.5],[1,1.5],[1,.5],[1,100]]) { const row={fn:"context",entries,index,n}; try {row.out=c.contextWindow(entries,index,n);} catch(e) {row.error=e.message;} cases.push(row); }
process.stdout.write(JSON.stringify(cases)+"\n");
