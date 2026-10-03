// Records what the CXC v0.2.40 oracle's config-guard/src/toml-edit.ts and text-lines.ts answer over the grids below; the Go tests
// replay oracle-toml-edit.json (no Node at test time) and every case must agree. Each case is {fn, in, out}: the oracle function of
// that name applied to the arguments, out being what it returned (setTableKey and restoreTableKey return their whole result object,
// findKeyLine returns an object, the string "unsupported" or null, readTableKey and tomlTableBody return a string or null). Recorded
// with Node v24.20.0 as
//   node record-toml-edit-oracle.mjs file://<oracle>/plugins/codexclaw/components/config-guard/dist > oracle-toml-edit.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d) holding its committed dist/.
const dist = process.argv[2];
const te = await import(dist + "/toml-edit.js");
const tl = await import(dist + "/text-lines.js");

const MEM = ["memories", "dedicated_tools"];
const V2 = ["features.multi_agent_v2", "enabled"];
const crlf = (s) => s.replace(/\n/g, "\r\n");

// Lines that are the inside of a multi-line string. The oracle edits or deletes them as if they were keys or headers (the loss class), so each such
// line that looks like one carries a MARK in front of it. The Go port must treat a line inside a string as not there: the "neutral/" rows
// record what the oracle answers for the text with the MARK left in (which no pattern matches), with the MARK removed from the answer.
const MARK = "§";
const LOSS = [
  "[memories]\nnote = \"\"\"\n§[memories]\n§dedicated_tools = false\n\"\"\"\n", "[memories]\nnote = '''\n§dedicated_tools = true\n'''\ndedicated_tools = false\n",
  "[memories]\nnote = \"\"\"\n§[features]\n\"\"\"\ndedicated_tools = false\n", "note = \"\"\"\n§[memories]\n§dedicated_tools = false\n\"\"\"\n",
  "[memories]\nnote = \"\"\"a \\\"\"\" b\n§dedicated_tools = false\n\"\"\" # c\ndedicated_tools = true\n",
  "[memories]\nnote = \"\"\"\n§dedicated_tools = false\n\"\"\"\n", "[memories]\nnote = \"\"\"\n  §dedicated_tools = false\n\n  §[x]\n\"\"\"\n[other]\nx = 1\n",
  "[memories]\na = [\"\"\"x\"\"\"\", \"\"\"\n§dedicated_tools = false\n\"\"\"]\ndedicated_tools = true\n[other]\nx = 1\n",
  "[memories]\na = ['''x'''', '''\n§dedicated_tools = false\n''']\ndedicated_tools = true\n[other]\nx = 1\n",
  "[memories]\na = \"\"\"x\\\"\"\"\n§dedicated_tools = false\n\"\"\"\ndedicated_tools = true\n", "[memories]\nnote = \"\"\"\n§dedicated_tools = false",
  "[memories]\nlist = [\"\"\"\n§[x]\n\"\"\", 'a']\ndedicated_tools = false\n",
  "[memories]\nnote = \"\"\"\n§dedicated_tools = false\n\"\"\"\ndedicated_tools = false\n", "[memories]\nnote = '''\n§[memories]\n'''\n[memories]\ndedicated_tools = false\n",
  "[memories]\nnote = \"\"\"\r\n§dedicated_tools = false\r\n\"\"\"\r\nx = 1\r\n",
  "[memories]\nnote = \"\"\"\n§[memories]\n\"\"\"\ndedicated_tools = false\n[features]\nmulti_agent = true\n",
];
const real = (s) => s.split(MARK).join("");
const unmark = (v) => (typeof v === "string" ? real(v) : Array.isArray(v) ? v.map(unmark) : v && typeof v === "object" ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, unmark(x)])) : v);
// LF texts; each also runs with every line ending turned into CRLF.
const BASES = [
  "", "\n", "[memories]\n", "[memories]", "[memories]\n\n\n",
  "[memories]\ngenerate_memories = true\nuse_memories = true\n",
  "[memories]\ndedicated_tools = false\n", "[memories]\ndedicated_tools = true\n", "[memories]\ndedicated_tools = false",
  "[memories]\ndedicated_tools = false  # note\n", "[memories]\ndedicated_tools=false#c\n",
  "[memories]\n  \tdedicated_tools\t=\tfalse \t\n", "[memories]\ndedicated_tools = 1\n", "[memories]\ndedicated_tools = TRUE\n",
  "[memories]\ndedicated_tools = \"true\" # c # d\n", "[memories]\ndedicated_tools = 'x # y' tail\n",
  "[memories]\ndedicated_tools = \"a\\\"b # c\"\n", "[memories]\ndedicated_tools = \"\\\\\" # c\n", "[memories]\ndedicated_tools = 'a\\' # c\n",
  "[memories]\ndedicated_tools = \"oops\n", "[memories]\ndedicated_tools = \"oops\\\n", "[memories]\ndedicated_tools = \"\"\"x\ny\"\"\"\n",
  "[memories]\ndedicated_tools = '''x'''\n", "[memories]\ndedicated_tools = [1, 2] # c\n", "[memories]\ndedicated_tools = { a = \"#\" }\n",
  "[memories]\ndedicated_tools =\n", "[memories]\ndedicated_tools\n", "[memories]\n# dedicated_tools = false\n",
  "[memories]\ndedicated_tools_x = 1\nxdedicated_tools = 2\n", "[memories]\ndedicated_tools.x = 1\n", "[memories]\n\"dedicated_tools\" = false\n",
  "[ memories ]\nx = 1\n", "[\"memories\"]\nx = 1\n", "[memories] # tuning\nx = 1\n", "[memories]#c\nx = 1\n", "[memories] x\ny = 1\n",
  "[memories]]\ny = 1\n", "  [memories]\n  x = 1\n", "[[memories]]\nx = 1\n", "memories.dedicated_tools = false\n",
  "\u00a0[memories]\u3000\n\u2003dedicated_tools\u00a0=\u00a0false\u00a0\n", "\ufeff[memories]\ndedicated_tools = false\n",
  "[memories]\v\ndedicated_tools = false\v\n", "[memories]\nx = 1\n[features]\nmulti_agent = true\n",
  "[memories]\nx = 1\n\n\n# trailing comment\n\n[features]\nmulti_agent = true\n", "[memories]\n# only a comment\n\n[features]\na = 1\n",
  "[features]\nmulti_agent = true\n", "model = \"gpt\"\n\n[features]\nhooks = true\n\n\n", "x = 1\n\n\n\n", "   \n\t\n",
  "[memories]\nlist = [\n  [1, 2],\n]\ndedicated_tools = false\n", "[memories]\nx = 1\nlist = [\n  [1, 2],\n]\n",
  "[memories]\nnote = \"\"\"\ndedicated_tools = false\n\"\"\"\n", "[memories]\nnote = \"\"\"\ndedicated_tools = false\n\"\"\"\ndedicated_tools = false\n",
  "[memories]\ndedicated_tools = false\ndedicated_tools = true\n", "[memories]\ndedicated_tools = false\n[memories]\ndedicated_tools = true\n",
  "[memories]\ndedicated_tools = \"日本語 😀\" # 주석 😀\n", "[memories]\ndedicated_tools = \"\\😀\" # c\n", "# 사용자 메모\n[memories]\n# 사용자가 남긴 메모\ndedicated_tools = true\n",
  "[features.multi_agent_v2]  # tuning\nenabled = true\nmax_concurrent_threads_per_session = 8\n",
  "[features]\nmulti_agent_v2 = { enabled = true }\n[features.multi_agent_v2]\nenabled = false\n", "[featuresXmulti_agent_v2]\nenabled = true\n",
  "[features.multi_agent_v2]\r\nenabled = true\r\n[other]\r\n",
  "[memories]\ndedicated_tools = #c\n", "[memories]\ndedicated_tools = # c\n", "\ufeff", "\ufeff\n", "x = 1\n\ufeff\n", "[memories]\nx = 1\n\ufeff\n\n[features]\n",
  // Delimiters that must not open or close a multi-line string (the answers equal the oracle's).
  "[memories]\nnote = \"\"\"\"quoted\"\"\"\"\ndedicated_tools = false\n", "[memories]\n# \"\"\" in a comment\ndedicated_tools = false\ns = \"\\\"\"\"\n",
  "[memories]\na = '\"\"\"'\ndedicated_tools = false\n", "[memories]\na = \"'''\"\ndedicated_tools = false\n", "[memories]\n# \"\"\"\ndedicated_tools = false\n",
  "[memories]\na = \"\"\"x\"\"\"\"\ndedicated_tools = false\n", "[memories]\na = ''''''\ndedicated_tools = false\n", "[memories]\na = \"\"\"\"\"\"\ndedicated_tools = false\n",
  "[memories]\na = \"\"\"x\"\"\"\"\"\ndedicated_tools = false\n", "[memories]\na = ['''x''''] # '''\ndedicated_tools = false\n", "[memories]\n\"a'''b\" = 1\ndedicated_tools = false\n",
];
const CRLF_OF = (b) => b.includes("\n") && !b.includes("\r");
const LOSS_ALL = [...LOSS, ...LOSS.filter(CRLF_OF).map(crlf)];
// Texts whose line endings are not uniform, and CR that is not part of a CRLF.
const RAW = [
  "[memories]\r\nx = 1\ny = 2\n", "[memories]\r\nx = 1\r\ny = 2\n", "[a]\r\nx = 1\r\n[memories]\ny = 2\n", "[memories]\nx = 1\r\ny = 2\r\n",
  "[memories]\ndedicated_tools = false\r", "[memories]\r\ndedicated_tools = false\r", "[memories]\r\nx = 1\r\r\ny = 2\r\n", "[memories] # c\r", "[memories]\r",
  "[memories]\nx = 1\rdedicated_tools = false\n", "[memories]\ndedicated_tools = false\u2028x\n", "[memories] # c\u2028x\n", "[memories]\u2028\nx = 1\n",
  "\r\n", "\r", "[memories]\r\n\r\n\r\n", "x = 1\r\n\r\n\r\n",
];
const CONTENTS = [...BASES, ...BASES.filter(CRLF_OF).map(crlf), ...RAW, ...LOSS_ALL.map(real)];
const unique = [...new Set(CONTENTS)];

const cases = [];
const add = (fn, args, out) => cases.push({ fn, in: args, out });
const lines = (c) => tl.splitLines(c);

for (const c of unique) {
  for (const value of [true, false]) add("setTableKey", [c, ...MEM, value], te.setTableKey(c, ...MEM, value));
  for (const prior of [null, "false", "\"x # y\""]) add("restoreTableKey", [c, ...MEM, prior], te.restoreTableKey(c, ...MEM, prior));
  add("readTableKey", [c, ...MEM], te.readTableKey(c, ...MEM));
  add("tomlTableBody", [c, MEM[0]], te.tomlTableBody(c, MEM[0]));
  const ls = lines(c);
  const h = te.findTableHeader(ls, MEM[0]);
  add("findTableHeader", [ls, MEM[0]], h);
  for (const at of new Set([-1, h, 0, 1, 99])) add("findKeyLine", [ls, at, MEM[1]], te.findKeyLine(ls, at, MEM[1]));
}
// A few texts get the rest of the prior values and the dotted multi_agent_v2 table.
for (const c of ["[memories]\ndedicated_tools = true\n", "[memories]\r\ndedicated_tools = true # c\r\n", "[memories]\ndedicated_tools = \"x\"\n", "[memories]\nx = 1\n", ""]) {
  for (const prior of ["true", "1", "", "'lit'", "[1]"]) add("restoreTableKey", [c, ...MEM, prior], te.restoreTableKey(c, ...MEM, prior));
}
for (const c of unique.filter((x) => /multi_agent_v2|features|^$/.test(x))) {
  add("setTableKey", [c, ...V2, true], te.setTableKey(c, ...V2, true));
  add("restoreTableKey", [c, ...V2, null], te.restoreTableKey(c, ...V2, null));
  add("readTableKey", [c, ...V2], te.readTableKey(c, ...V2));
  add("tomlTableBody", [c, V2[0]], te.tomlTableBody(c, V2[0]));
  add("tomlTableBody", [c, "features"], te.tomlTableBody(c, "features"));
  add("findTableHeader", [lines(c), V2[0]], te.findTableHeader(lines(c), V2[0]));
}
// Headers and keys holding regular-expression metacharacters, against the text that names them and a near miss with the character replaced.
for (const t of ["a.b", "a+b", "a*b", "a?b", "a^b", "a$b", "a(b)", "a|b", "a[b]", "a{b}", "a\\b", "a-b", "a b", "a/b"]) {
  const near = t.replace(/[.+*?^$()|[\]{}\\ \/-]/, "X");
  for (const c of ["[" + t + "]\nk = false\n", "[" + near + "]\nk = false\n", "[x]\n" + t + " = 1\n[" + t + "]\n" + t + " = false\n"]) {
    add("setTableKey", [c, t, "k", true], te.setTableKey(c, t, "k", true));
    add("setTableKey", [c, "x", t, true], te.setTableKey(c, "x", t, true));
    add("readTableKey", [c, t, t], te.readTableKey(c, t, t));
    add("tomlTableBody", [c, t], te.tomlTableBody(c, t));
    add("findTableHeader", [lines(c), t], te.findTableHeader(lines(c), t));
  }
}
const addNeutral = (fn, realArgs, markedArgs) => cases.push({ fn: "neutral/" + fn, in: realArgs, out: unmark(te[fn](...markedArgs)) });
for (const L of LOSS_ALL) {
  const r = real(L);
  for (const value of [true, false]) addNeutral("setTableKey", [r, ...MEM, value], [L, ...MEM, value]);
  for (const prior of [null, "false", "\"x # y\"", ""]) addNeutral("restoreTableKey", [r, ...MEM, prior], [L, ...MEM, prior]);
  addNeutral("readTableKey", [r, ...MEM], [L, ...MEM]);
  for (const t of ["memories", "features", "other"]) addNeutral("tomlTableBody", [r, t], [L, t]);
  addNeutral("findTableHeader", [lines(r), MEM[0]], [lines(L), MEM[0]]);
  const h = te.findTableHeader(lines(L), MEM[0]);
  for (const at of new Set([-1, h, 0, 1, 2, 99])) addNeutral("findKeyLine", [lines(r), at, MEM[1]], [lines(L), at, MEM[1]]);
}
// A one-line string, a comment or a longer closing run must not open or close a multi-line string: these texts equal the oracle row for row.
// Every JavaScript whitespace character and some look-alikes (U+0085, U+180E, U+200B, U+2060) at each place the two patterns and the
// trims treat them: before and after a header, before a key, around the equals sign, around a value and in front of a comment.
const SPACES = ["\t", "\v", "\f", " ", "\u00a0", "\u1680", "\u2000", "\u2005", "\u200a", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", "\ufeff", "\r", "\u0085", "\u180e", "\u200b", "\u2060", " \u00a0 "];
const MORE_SPACES = ["\n", "\u2001", "\u2002", "\u2003", "\u2004", "\u2006", "\u2007", "\u2008", "\u2009"];
for (const ch of [...SPACES, ...MORE_SPACES]) {
  for (const h of [ch + "[t]", "[t]" + ch, "[t]" + ch + "# c", "[t]# c" + ch, "[t] #" + ch + "c"]) {
    add("findTableHeader", [[h], "t"], te.findTableHeader([h], "t"));
    const c = h + "\nk = false\n";
    add("setTableKey", [c, "t", "k", true], te.setTableKey(c, "t", "k", true));
    add("tomlTableBody", [c, "t"], te.tomlTableBody(c, "t"));
  }
  const keys = [ch + "k = true", "k" + ch + "= true", "k =" + ch + "true", "k = true" + ch, "k = true" + ch + "# c", "k = \"x\"" + ch + "# c", "k = \"x\"" + ch, "k = true # c" + ch, "k =" + ch, "k = 'a'" + ch + ch + "#" + ch];
  for (const k of keys) {
    add("findKeyLine", [["[t]", k], 0, "k"], te.findKeyLine(["[t]", k], 0, "k"));
    const c = "[t]\n" + k + "\n[u]\nk = 1\n";
    add("setTableKey", [c, "t", "k", true], te.setTableKey(c, "t", "k", true));
    add("setTableKey", [c, "t", "k", false], te.setTableKey(c, "t", "k", false));
    add("restoreTableKey", [c, "t", "k", null], te.restoreTableKey(c, "t", "k", null));
    add("readTableKey", [c, "t", "k"], te.readTableKey(c, "t", "k"));
  }
  for (const l of [ch + "[u]", ch + "[[u]]", "[u]" + ch]) {
    const c = "[t]\nx = 1\n" + l + "\nk = 1\n";
    add("tomlTableBody", [c, "t"], te.tomlTableBody(c, "t"));
    add("setTableKey", [c, "t", "k", true], te.setTableKey(c, "t", "k", true));
  }
}
// An empty key against an indent, and header indexes outside the lines.
for (const ls of [["[t]", "   = false"], ["[t]", "= false"], ["[t]", " \t= x # c"]]) add("findKeyLine", [ls, 0, ""], te.findKeyLine(ls, 0, ""));
for (const at of [-3, -2, -1, 0, 1, 2, 3, 4, 5]) for (const ls of [["[t]", "k = 1", "[u]", "k = 2"], ["k = 1"], []]) add("findKeyLine", [ls, at, "k"], te.findKeyLine(ls, at, "k"));
for (const prior of ["", "x\ny", "x\r\ny", " "]) for (const c of ["[memories]\ndedicated_tools = true\n", "[memories]\r\ndedicated_tools = true\r\n", "[memories]\r\nx = 1\ndedicated_tools = true\n"]) {
  add("restoreTableKey", [c, ...MEM, prior], te.restoreTableKey(c, ...MEM, prior));
}
// The oracle's text-lines.ts; internal/pabcd/text must give the same answers.
for (const s of ["", "a", "a\nb", "a\r\nb", "a\r\nb\nc", "a\r\nb\r\nc\nd", "a\nb\r\n", "a\rb", "a\r", "\r\n", "\n", "\r", "a\r\r\nb", "\r\n\r\n\n", "a\u2028b", "😀\r\n😀", "no newlines at all", "alpha\r\nbeta\ngamma\r\n", "a\nb"]) {
  add("splitLines", [s], tl.splitLines(s));
  add("splitLinesByteExact", [s], tl.splitLinesByteExact(s));
  add("dominantEol", [s], tl.dominantEol(s));
  for (const eol of ["\n", "\r\n"]) add("withEol", [s, eol], tl.withEol(s, eol));
}
process.stdout.write(JSON.stringify(cases) + "\n");
