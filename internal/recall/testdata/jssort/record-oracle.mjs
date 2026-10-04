// Node v24.20.0 / V8 13.6.233.17-node.53; CXC v0.2.40 (3c1459ac).
// node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist <scratch>
// Only the scratch dist copy gets a private-helper export. Go replays the JSON offline.
import { cpSync, mkdtempSync, appendFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
if (process.versions.node !== "24.20.0") throw Error("record with Node v24.20.0");
const root = mkdtempSync(join(process.argv[3], "sort-oracle-"));
cpSync(fileURLToPath(process.argv[2]), join(root, "dist"), { recursive: true });
writeFileSync(join(root, "dist/package.json"), '{"type":"module"}\n');
appendFileSync(join(root, "dist/memory-search.js"), "\nexport { rankAndTrim };\n");
const m = await import(pathToFileURL(join(root, "dist/memory-search.js")));
const encode = n => Number.isNaN(n) ? "nan" : n === Infinity ? "infinity" : n === -Infinity ? "-infinity" : Object.is(n, -0) ? "-0" : n;
let state = 526;
const random = () => state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
const shuffle = a => { for (let i = a.length - 1; i > 0; --i) { const j = random() % (i + 1); [a[i], a[j]] = [a[j], a[i]]; } return a; };
const ids = n => Array.from({ length: n }, (_, i) => i);
const sorts = [];
const record = (name, mode, input, gen, table = []) => {
  const trace = [], out = [...input];
  let calls = 0;
  out.sort((a, b) => {
    trace.push([a, b]); ++calls;
    switch (mode) {
      case "asc": return a - b;
      case "desc": return b - a;
      case "zero": return -0;
      case "negative": return -1;
      case "positive": return 1;
      case "nan": return NaN;
      case "cycle": return a % 3 === b % 3 ? 0 : (a + 1) % 3 === b % 3 ? -1 : 1;
      case "table": return table[a % table.length][b % table.length];
      case "stateful": return calls % 7 === 0 ? NaN : calls % 3 === 0 ? 0 : calls % 2 ? -1 : 1;
      default: throw Error(mode);
    }
  });
  // Full traces retain small and branch witnesses; larger schedules are still
  // checked in full through SHA-256 of UTF-8 JSON.stringify(trace), plus count.
  const full = input.length <= 33 || gen.kind === "runs" || ["asymmetric-3", "asymmetric-5", "stateful-129", "cycle-129"].includes(name);
  const evidence = full ? { trace } : { traceDigest: [...createHash("sha256").update(JSON.stringify(trace)).digest()] };
  sorts.push({ name, mode, gen, out, traceCount: trace.length, ...evidence });
};
for (const n of [0, 1, 2, 3, 7, 31, 32, 33, 63, 64, 65, 127, 128, 129, 511, 1024, 4096]) {
  for (const mode of ["asc", "desc", "zero", "negative", "positive", "nan", "cycle", "stateful"]) {
    const seed = state; // Capture BEFORE the argument's shuffle advances state.
    record(`${mode}-${n}`, mode, shuffle(ids(n)), { kind: "shuffle", n, seed });
  }
}
for (const [a, b] of [[100, 100], [192, 64], [64, 192], [1000, 1000]]) {
  record(`natural-${a}-${b}`, "asc", [...ids(a).map(i => i + b), ...ids(b)], { kind: "runs", lengths: [a, b] });
}
record("natural-singleton-tail", "asc", [...ids(100).map(i => i + 1), 0], { kind: "runs", lengths: [100, 1] });
record("collapse-third-last", "asc", [...ids(100).map(i => i + 190), ...ids(70).map(i => i + 120), ...ids(120)], { kind: "runs", lengths: [100, 70, 120] });
const numbers = [-Infinity, -2, -1, -0, 0, 1, 2, Infinity, NaN];
for (let i = 0; i < 300; ++i) {
  // asymmetric-3 and asymmetric-5 witness MergeHigh/Low exhausted-run exits.
  const seed = state;
  const table = Array.from({ length: 13 }, () => Array.from({ length: 13 }, () => numbers[random() % numbers.length]));
  const n = [33, 64, 65, 128, 257, 513][i % 6];
  record(`asymmetric-${i}`, "table", shuffle(ids(n)), { kind: "table", n, seed }, table);
}
// The callback sees the receiver unchanged until V8 copies the work array back.
const snapshot = [5, 1, 4, 2, 3], visible = [];
snapshot.sort((a, b) => { visible.push([...snapshot]); return a - b; });
const ranks = [];
const rank = (name, input, limit, gen) => {
  const candidates = input.map(h => ({ ...h }));
  const out = m.rankAndTrim(candidates, limit);
  const inline = gen.kind === "inline" ? { input: input.map(h => ({ score: encode(h.score), updatedAt: h.updatedAt })) } : {};
  ranks.push({ name, gen, ...inline, limit: encode(limit), out: out.map(h => Number(h.excerpt.slice(1))), sorted: candidates.map(h => Number(h.excerpt.slice(1))) });
};
const hit = (i, score, updatedAt = null, relpath = `f${i}`) => ({ origin: "file", kind: "other", relpath, threadId: null, updatedAt, excerpt: `h${i}`, startLine: i + 1, cwd: null, score });
for (const limit of [-Infinity, -1, -0, 0, 0.5, 1, 2, 2.5, 20, Infinity, NaN]) {
  rank(`cap-limit-${encode(limit)}`, ids(12).map(i => hit(i, 12 - i, null, `f${i % 3}`)), limit, { kind: "cap", n: 12 });
  rank(`empty-${encode(limit)}`, [], limit, { kind: "empty", n: 0 });
}
for (const score of numbers) for (const date of [null, "", "2026-01-01"]) rank(`tie-${encode(score)}-${date}`, ids(65).map(i => hit(i, score, date)), 20, { kind: "tie", n: 65, score: encode(score), date });
rank("utf16-dates", [hit(0, 1, "\uffff"), hit(1, 1, "😀"), hit(2, 1, "a"), hit(3, 1, null), hit(4, 1, "")], 20, { kind: "inline", n: 5 });
for (let i = 0; i < 300; ++i) {
  const seed = state, n = [3, 32, 33, 64, 129, 257][i % 6];
  const input = ids(n).map(j => hit(j, numbers[random() % numbers.length], [null, "", "2026-01-01", "2026-02-01"][random() % 4], `f${random() % 17}`));
  rank(`mixed-${i}`, input, [-1, 1, 2.5, 20, Infinity, NaN][i % 6], { kind: "mixed", n, seed });
}
process.stdout.write(JSON.stringify({ format: 2, node: process.versions.node, v8: process.versions.v8, seed0: 526, lcg: [1664525, 1013904223], sorts, snapshot: { input: [5, 1, 4, 2, 3], out: snapshot, visible }, ranks }) + "\n");
