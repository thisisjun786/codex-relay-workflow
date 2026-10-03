// Records what the CXC v0.2.40 oracle's recall/src/query-words.ts and synonyms.ts answer over the grids below; the Go tests
// replay oracle-query-words.json (no Node at test time) and every case must agree. Each case is {fn, in, out}: the oracle function
// of that name applied to the arguments. Offsets are the oracle's UTF-16 code-unit indexes. Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist > oracle-query-words.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
const dist = process.argv[2];
const q = await import(dist + "/query-words.js");
const s = await import(dist + "/synonyms.js");

const SPLIT = [
  "", "   ", "  CI  PR3956 ", "a b c d e f g h i j", "x\ty\nz\vw\fv\ru", "a\u00a0b\u1680c\u2003d\u2028e\u2029f\u202fg\u205fh\u3000i\ufeffj",
  "a\u0085b\u180ec\u200bd\u200ee", "İSTANBUL ΑΣ Σ ΑΣ. K", "지난번 로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인한 방법",
  Array.from({ length: 21 }, (_, i) => "w" + i).join(" "), "a😀b 😀", "Ａ１ ２．４９",
];
const WORDS = [
  "CI", "PR", "FTS", "RRF", "LSP", "ABCDEF", "ABCDEFG", "A", "go", "id", "ci", "abc", "abcd", "3956", "#3956", "1", "#1", "12", "#12",
  "1234567890", "12345678901", "#1234567890", "6e97e73d", "6e97e7", "6E97E73D", "deadbeef", "abcdef0", "defaced", "hook.ts", "plan.md",
  "package.json", "src/hook.ts", "a\\b", "x/", "2.49.0", "2.49", "2.49.0-rc.1", "v2.49.0", "V2.49.0", "2.49.", "2.", "v2", "2.49.0-",
  "2.49.0-RC.1", "2.49.0-rc_1", "2.49.0.1", "1.2.3.4.5", "v", "2.49.0a", "v2.49.0-İ", "2.49.0-rc.", "deploy", "release", "Codex",
  "BundledPluginsMarketplace", "NaiControlsPanel", "PR3956", "HTTP2", "npm", "a1B", "A1b", "1Ab", "aB", "Ab", "The", "and", "검색", "배포까지",
  "트라이그램", "배포하고", "코덱스를", "지난번", "확인한", "e.g", "etc.", "a.b", "a.", "a..b", ".a", ".gitignore", "file.abcde", "file.abcdef",
  "foo.TS", "foo.İ", "İ", "İD", "K", "k", "ΑΣ", "Σ", "ß", "ẞ", "İSTANBUL", "Ａ", "ＡＢ", "٣٤٥", "２.４９", "😀", "a😀b", "é", "Éa", "그", "이",
  "저", "것", "문제", "방법", "문제를", "",
];
const KOREAN = [
  "배포까지", "문서를", "인덱스도", "스킬들을", "세션에서는", "결정했지", "배포하고", "검색해야", "검사", "고의", "하고", "이해", "오해", "릴리스", "도구",
  "deploy", "hook.ts", "배포2", "소스인지", "무엇인지", "인지", "검증한", "해결했지", "해결하다", "해석하는", "재시작하면", "하하하하하", "배포하하하하하",
  "하배포해", "코덱스를", "도그푸딩", "배포를", "배포했다", "인덱스도", "기억", "메모리를", "에서는", "으로는", "한", "가", "을", "배포 ", "배포ㄱ", "Ａ", "배포\u0301",
  "스킬들이", "스킬에게", "스킬이나", "스킬부터", "스킬처럼", "스킬보다", "스킬마다", "스킬라고", "스킬해서", "스킬하는", "스킬한테", "스킬에는", "스킬으로", "스킬에서",
  "스킬은", "스킬는", "스킬의", "스킬에", "스킬도", "스킬과", "스킬와", "스킬만", "스킬로", "스킬이", "스킬가", "스킬를", "스킬을", "스킬하", "스킬해", "스킬했",
  "스킬했다가요", "스킬했다가요요", "스킬해보자고요", "스킬하하하하", "가나다라하", "가나다하라마", "가나해다라마", "집에서는", "오하해하", "결정하하하하",
];
const TEXTS = [
  "see hook.ts for details", "edit src/hook.ts now", "edit my-hook.tsx now", "ci", "run ci", "ci를 돌렸다", "ci_runner", "ci2", "nothing here", "",
  "precision then ci", "precision ci ci_x ci", "cici cici", "ci ci ci ci ci ci", "slsa provenance, v2.49.0", "released (v2.49.0) today", "2.49.0",
  "av2.49.0", "12.49.0", "2.49.00", "vci runner", "가나다 ci 라마", "😀 ci", "ci😀ci", "😀😀 v2.49.0 가", "naicontrolspanel and naicontrolspanel again",
  "the lsp server restarted", "shipped pr3956 to production", "shipped pr #3956 today", "v2.49.0.", "xv2.49.0", "-v2.49.0", "_v2.49.0", "v v2.49.0",
  "vv2.49.0", "(v2.49.0)", "배포까지 했다", "배포", "ṽ2.49.0", "2.49.0-rc.1 and v2.49.0-rc.1", "hook.ts hook.tsx hook.ts", "i̇ci", "ǅci", "v2.49.0v2.49.0",
];
const TERMS = [
  ["ci", true], ["ci", false], ["hook.ts", true], ["hook.ts", false], ["src/hook.ts", true], ["2.49.0", true], ["2.49.0", false], ["v2.49.0", true],
  ["lsp", true], ["3956", true], ["3956", false], ["배포", true], ["배포", false], ["", true], ["", false], ["😀", true], ["ci ci", true], ["2.49", true],
  ["2.49.0-rc.1", true], ["v", true], ["c", true], ["i̇", true],
].map(([text, boundary]) => ({ text, boundary }));
const STOP = [
  ["CI", "문제"], ["재시작", "문제", "방법"], ["그", "문제"], ["이해", "문제"], [], ["그", "이", "저", "것", "문제", "방법"], ["그", "CI", "이"], ["Codex", "그"],
  ["2.49.0", "방법"], ["문제를", "문제"], ["그", "그"], ["İ", "이"], ["이", "ΑΣ"], ["그", "go"], ["그", "그Ab"], ["방법", "Codex"],
];
const QUERIES = [
  "", "   ", "CI", "trigram korean", "2.49.0 SLSA", "3956 LSP", "plan audit", "결정 세션", "지난번 로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인한 방법",
  "코덱스를 재시작하면 플러그인이 사라지는 문제", "2.49.0 배포하고 npm 패키지가 진짜 그 소스인지 검증한 기록", "그 이 저 것 문제 방법", "a b c d e f g h", "a b c d e f g h i",
  "배포까지", "배포를", "기억", "LSP", "fts", "go id ci", "hook.ts src/hook.ts", "Codex BundledPluginsMarketplace npm 배포하고 확인한 기록 문서를 스킬들을 세션에서는 결정했지",
  "zxqv84721무지개잠수함", "memory search review branch", "v2.49.0 2.49 Codex",
];
const PLAN_TEXTS = [
  "2.49.0 npm 패키지가 검증한 기록", "2.49.0 npm 패키지가 검증한 내용", "2.49.0 npm 패키지가 다른 내용", "배포 npm 패키지가 검증한 기록", "bun link healthz local source service",
  "the lsp server crashed", "naicontrolspanel notes", "shipped pr3956 today", "shipped pr #3956 today", "we ran the audit twice", "the memory store keeps every decision",
  "codex restart plugin wipe bundledpluginsmarketplace", "배포까지 했다", "첫 배포 이후 인덱스 재생성", "v2.49.0 slsa provenance", "", "trigram korean", "run ci now", "hook.ts changed", "2.49.0 shipped deploy",
];
const RELAX = [["LSP"], ["배포"], ["3956", "LSP"], ["CI", "배포를", "hook.ts"], ["2.49.0", "plan", "audit"], []];
const RELAX_AT = [[], [0], [1], [0, 2], [5]];

const t = (x) => ({ text: x.text, boundary: x.boundary });
const groupsJSON = (gs) => gs.map((g) => g.map(t));
const planJSON = (p) => ({ required: groupsJSON(p.required), optional: groupsJSON(p.optional), minOptional: p.minOptional, anyMode: p.anyMode });
const cases = [];
const add = (fn, args, out) => cases.push({ fn, in: args, out });

add("consts", [], [q.MAX_WORDS, q.MAX_QUERY_TERMS]);
add("stopwords", [], [...q.QUERY_STOPWORDS]);
add("synonymGroups", [], s.SYNONYM_GROUPS);
for (const query of SPLIT) { add("splitRaw", [query], q.splitQueryWordsRaw(query)); add("split", [query], q.splitQueryWords(query)); }
for (const w of new Set([...WORDS, ...KOREAN])) {
  add("symbol", [w], q.isSymbolWord(w)); add("version", [w], q.isVersionWord(w)); add("required", [w], q.isRequiredTerm(w)); add("stem", [w], s.koreanStem(w));
}
const EXPAND = [...new Set([...WORDS, ...KOREAN, ...s.SYNONYM_GROUPS.flat()])].filter((w) => w !== "").map((w) => [w]);
EXPAND.push([""]);
EXPAND.push(["결정", "quagga"], ["plan", "audit"], ["배포를", "CI", "2.49.0"], s.SYNONYM_GROUPS.map((g) => g[0]), []);
for (const words of EXPAND) add("expand", [words], groupsJSON(s.expandQueryWords(words)));
for (const text of TEXTS) for (const term of TERMS) {
  add("term", [text, term], [q.termIndexOf(text, term), q.termIncludes(text, term), q.countTermOccurrences(text, term, 5), q.countTermOccurrences(text, term, 1), q.countTermOccurrences(text, term, 0)]);
}
for (const words of STOP) add("dropStopwords", [words], q.dropStopwords(words));
for (const query of QUERIES) {
  const rawAll = q.splitQueryWordsRaw(query);
  const raw = q.dropStopwords(rawAll);
  for (const any of [false, true]) for (const syn of [false, true]) {
    // chat-search.ts groupsForChat/chatMatchPlan: boundary gating off on every term.
    const chatGroups = syn ? s.expandQueryWords(raw).map((g) => g.map((x) => ({ text: x.text, boundary: false }))) : raw.map((w) => [{ text: w.toLowerCase(), boundary: false }]);
    // memory-search.ts: expansion on by default, the first member only when it is off.
    const memGroups = syn ? s.expandQueryWords(raw) : s.expandQueryWords(raw).map((g) => [g[0]]);
    for (const [kind, groups] of [["chat", chatGroups], ["memory", memGroups]]) {
      const plan = q.compileMatchPlan(groups, raw, any, rawAll.length > q.MAX_WORDS);
      add("plan", [kind, query, any, syn, PLAN_TEXTS], { plan: planJSON(plan), empty: q.planIsEmpty(plan), all: groupsJSON(q.allGroups(plan)), hits: PLAN_TEXTS.map((x) => q.planMatches(x, plan)) });
    }
  }
}
{
  const groups = groupsJSON(s.expandQueryWords(["CI", "배포", "Codex"]));
  for (const raw of [["CI"], [], ["CI", "배포", "Codex", "x"]]) for (const any of [false, true]) add("compile", [groups, raw, any], planJSON(q.compileMatchPlan(groups, raw, any, true)));
}
{
  const [ci, bae, v, lsp, hook] = [[["ci", true]], [["배포", false], ["deploy", false]], [["2.49.0", true]], [["lsp", true]], [["hook.ts", true]]].map((g) => g.map(([text, boundary]) => ({ text, boundary })));
  const hand = [
    { required: [], optional: [ci, bae], minOptional: 1, anyMode: true }, { required: [ci], optional: [bae], minOptional: 1, anyMode: true },
    { required: [], optional: [ci, bae, v], minOptional: 0, anyMode: false }, { required: [v], optional: [ci, bae], minOptional: 0, anyMode: false },
    { required: [], optional: [], minOptional: 0, anyMode: true }, { required: [ci], optional: [], minOptional: 5, anyMode: false },
    { required: [], optional: [ci, bae, lsp], minOptional: 3, anyMode: false }, { required: [hook], optional: [bae], minOptional: 2, anyMode: false },
  ];
  for (const plan of hand) add("planMatches", [plan, PLAN_TEXTS], PLAN_TEXTS.map((x) => q.planMatches(x, plan)));
  // an empty group, and a raw word list shorter than the groups
  for (const [groups, raw] of [[[[]], []], [[[], ci], []], [[ci, []], ["ci"]]]) add("compile", [groups, raw, false], planJSON(q.compileMatchPlan(groups, raw, false, true)));
}
// the longest group any member or member-plus-ending word expands to (the 8-member cap is never reached)
{
  const ends = ["", "에서는", "으로는", "에서", "으로", "에는", "이나", "까지", "부터", "처럼", "보다", "마다", "라고", "하고", "해서", "하는", "한테", "인지", "들을", "들이", "에게", "을", "를", "이", "가", "은", "는", "의", "에", "도", "과", "와", "만", "로", "했다", "하면"];
  let max = 0;
  for (const m of s.SYNONYM_GROUPS.flat()) for (const e of ends) max = Math.max(max, s.expandQueryWords([m + e])[0].length);
  add("maxGroup", [ends], max);
}
// non-zero and negative start offsets, ASCII texts only (a start offset is in UTF-16 units here and in bytes in Go)
for (const text of TEXTS.filter((x) => /^[\x00-\x7f]*$/.test(x))) for (const term of TERMS.slice(0, 9)) for (const from of [-5, 3, 100]) add("termFrom", [text, term, from], q.termIndexOf(text, term, from));
for (const words of RELAX) {
  const groups = s.expandQueryWords(words);
  const relaxed = q.relaxQueryGroups(groups);
  add("relax", [words, RELAX_AT], [q.hasBoundaryTerm(groups), q.hasBoundaryTerm(relaxed), groupsJSON(relaxed), groups.map(q.groupTexts), RELAX_AT.map((idx) => groupsJSON(q.relaxGroupsAt(groups, new Set(idx))))]);
}
process.stdout.write(JSON.stringify(cases) + "\n");



