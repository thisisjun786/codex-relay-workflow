// Records what the CXC v0.2.40 oracle's attest.ts and plan-gate.ts answer over the grids below; the Go tests replay
// oracle-attest.json and oracle-plan-gate.json (no Node at test time). Inputs are kept as raw JSON text so a value JSON
// cannot carry through an object (1e999, -0) reaches both sides unchanged. Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> <out dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d). Plan-gate trees are
// built under <work dir>/r<random>/ws; the case root and the cwd are written back as ${ROOT} and ${CWD}.
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync, chmodSync } from "node:fs";
import { join } from "node:path";
const [oracleDist, workRoot, outDir] = process.argv.slice(2);
const A = await import(oracleDist + "/attest.js");
const G = await import(oracleDist + "/plan-gate.js");
mkdirSync(workRoot, { recursive: true });
mkdirSync(outDir, { recursive: true });

// Reasons are long and repeat across the grid: the file keeps each distinct text once in "texts" and refers to it by index.
const texts = [], seen = new Map();
const T = (s) => { if (!seen.has(s)) { seen.set(s, texts.length); texts.push(s); } return seen.get(s); };
const verdict = (r) => r === undefined ? null : { ok: r.ok, ...(r.reason !== undefined ? { reason: T(r.reason) } : {}), ...(r.reasons !== undefined ? { reasons: r.reasons.map(T) } : {}) };
const obj = (o) => JSON.stringify(o);
const withKey = (base, key, raw) => obj(base).slice(0, -1) + "," + JSON.stringify(key) + ":" + raw + "}";
const attest = [], tails = [], bindings = [];
let n = 0;
const record = (from, to, input, mode) => {
  const parsed = JSON.parse(input);
  const att = mode === "coerce" ? A.coerceAttest(parsed) : parsed;
  attest.push({ id: "c" + n++, mode, from, to, input, att: att === null ? null : JSON.parse(JSON.stringify(att)), validate: verdict(A.validateAttest(from, to, att)) });
};
const both = (from, to, input) => { record(from, to, input, "coerce"); record(from, to, input, "direct"); };

// ungated edges, with no attestation or a wrong one
for (const [f, t] of [["IDLE", "P"], ["C", "B"], ["C", "P"], ["D", "IDLE"], ["I", "P"], ["B", "A"], ["Z", "Q"]]) { both(f, t, "null"); both(f, t, obj({ from: "X", to: "Y" })); }
// null, wrong edge and wrong shape on each gated edge
for (const [f, t] of [["P", "A"], ["A", "B"], ["B", "C"], ["C", "D"]]) {
  record(f, t, "null", "coerce");
  for (const raw of ["[]", '"x"', "5", "true", "{}", obj({ from: 1, to: t, did: "x" }), obj({ from: f, to: t }), obj({ from: t, to: f, did: "real work" }), obj({ from: "C", to: "D", did: "real work" }), obj({ from: " " + f, to: t, did: "real work" })]) record(f, t, raw, "coerce");
  const edge = { from: f, to: t };
  // placeholder did words, only on the edges where nothing else can refuse
  if (f === "P" || f === "B") for (const did of ["", "tbd", "TBD", "ToDo", "n/a", "N/A", "na", "NA", "none", "None", "done", "Done", "ok", "OK", "oK", "o\u212a", "O\u212A", "...", ".", ". .", "-", "---", "--x", "\u2014", "  ", "tbd.", "n//a", "real work", "okay", "d\u00f3ne"]) both(f, t, obj({ ...edge, did }));
  for (const did of [" tbd ", "\u00a0tbd\u00a0", "\ufefftbd", "\u0085tbd", "tbd\n"]) both(f, t, obj({ ...edge, did }));
}
// A>B field combinations
const outputs = [undefined, "", "reviewed\nVERDICT: PASS", "VERDICT: FAIL"];
const verdicts = [undefined, "pass", "PASS", "Near-Pass", "NEAR-PASS", "fail", "Fail", "bogus", "", "FA\u0130L", "\u0391\u03a3", "\u03a3\u0391", "\u0391\u03a3.", "\u03a3", "\u0391\u03a3\u0391", "p\u0391ss", "\u212aa",
  // Final_Sigma context: Case_Ignorable members between the sigma and its cased neighbour, and what counts as cased
  "A'\u03a3", "A\u03a3'A", "A\u03a3\u00b7", "A\u03a3\u2019", "A\u03a3\u00ad", "A\u0301\u03a3", "\u03a3\u0301", "a.\u03a3", "1\u03a3", "\u2160\u03a3", "\u1d2c\u03a3", "\u24b6\u03a3", "A\u03a3\u1d2c", "A\u03a3\u2160", "A\u03a3\u0301A", "\u03a3\u03a3", "A\u03a3\u03a3", "A\u0387\u03a3", "A\u2027\u03a3", "A:\u03a3", "A\u03a3:A", "A\u03a3 ", "\u01c5\u03a3", "\u02b0\u03a3"];
const residuals = [undefined, "folded"];
for (const o of outputs) for (const v of verdicts) for (const r of residuals) {
  let s = obj({ from: "A", to: "B", did: "audited" });
  for (const [k, val] of [["auditOutput", o], ["auditVerdict", v], ["auditResidual", r]]) if (val !== undefined) s = s.slice(0, -1) + "," + JSON.stringify(k) + ":" + JSON.stringify(val) + "}";
  record("A", "B", s, "coerce");
  if (v === undefined || ["pass", "fail", "bogus", "near-pass", "PASS"].includes(v)) record("A", "B", s, "direct");
}
for (const t of ["x\nVERDICT: FAIL", "VERDICT: FAILED", "VERDICT: FAILURE", "VERDICT: FAIL-SAFE", "**VERDICT: FAIL**", "Verdict - FAIL", "verdict : fail", "verdict=fail", "VERDICT:FAIL", "VERDICT: FAIL.", "VERDICT: FAIL\n\n\n", "  VERDICT: FAIL  ", "VERDICT: FAIL\n(notes)", "VERDICT: FAIL\nVERDICT: PASS\nVERDICT: PASS", "VERDICT: FAIL\r\nround 2\r\nVERDICT: PASS", "a\r\nVERDICT: FAIL\r\n", "VERDICT: FAIL\n1\n2\n3\n4", "VERDICT: FAIL\n1\n2\n3\n4\n5", "x\n\u00a0VERDICT: FAIL", "\ufeffVERDICT: FAIL", "VERDICT:\u00a0FAIL", "VERDICT\u2003=\u2003fail", "VERDICT: FAIL\u00e9", "VERDICT: FAIL_", "VERDICT: FAIL1", "VERDICT: fail\u00a0", "VERDICT: FA\u0130L", "VERDICT: \ufb01", "verdict:\nFAIL", "VERDICT: PASS\nVERDICT: FAIL", "VERDICT: GO-WITH-FIXES (blockers=1)", "x\u2028VERDICT: FAIL", "x\rVERDICT: FAIL", "VERDICT: fail \nnotes\nmore\nlines\nhere\nand\nmore", "\u0085VERDICT: FAIL"]) {
  tails.push({ input: JSON.stringify(t), fail: A.hasFailVerdictTail(t) });
  both("A", "B", obj({ from: "A", to: "B", did: "audited", auditOutput: t, auditVerdict: "pass" }));
}
// C>D: checkOutput x exitCode x receipt path
const exits = [undefined, "0", "-0", "1", "1.5", "-1", "1e20", "1e21", "1e-7", "0.000001", "123456789012345680000", '"0"', "null", "true", "1e999", "-1e999", "2.5e-7", "100", "255", "0.1"];
for (const c of [undefined, "", "ok", "  "]) for (const e of exits) for (const r of [undefined, "r.json"]) {
  let s = obj({ from: "C", to: "D", did: "ran" });
  if (c !== undefined) s = withKey(JSON.parse(s), "checkOutput", JSON.stringify(c));
  if (e !== undefined) s = withKey(JSON.parse(s), "exitCode", e).replace(/^\{.*$/, (x) => x);
  if (r !== undefined) s = s.slice(0, -1) + ',"testReceiptPath":' + JSON.stringify(r) + "}";
  record("C", "D", s, "coerce");
  if (!["1e999", "-1e999", '"0"', "null", "true", "-0"].includes(e)) record("C", "D", s, "direct");
}
record("C", "D", obj({ from: "C", to: "D", did: "tbd", checkOutput: "", exitCode: 3 }), "coerce");
// coerce shapes
const E = { from: "A", to: "B", did: "x" };
for (const [k, vals] of Object.entries({
  did: ["  trimmed \u00a0", "\ufeff x \ufeff", "\u0085x\u0085", 5, null, [], {}], auditOutput: ["  t  ", 42, null, ""], auditVerdict: [" NEAR-PASS ", 5, "\u0130", "\u03a3", "A\u03a3 B"], auditResidual: [" r ", 1], auditRounds: [2, 2.5, "2", -1, 0, 1e21, null],
  checkOutput: [" c ", 7], override: [true, false, "yes", 1, null], planUnit: ["  devlog/_plan/260714_slug  ", 7, "", null], workPhaseId: [" wp2 ", 7, "", null], testReceiptPath: [" r.json ", 7, ""],
  planPaths: [["  a/b.md ", 42, "", null, "c"], "not-an-array", [], [1, 2], [" "], {}], exitCode: [0, 1.5, "1", null, true], extra: [1], from: [" A"],
})) for (const v of vals) record("A", "B", obj({ ...E, [k]: v }), "coerce");
for (const raw of ["1e999", "-1e999", "1E2", "-0"]) { record("C", "D", withKey({ from: "C", to: "D", did: "ran", checkOutput: "ok" }, "exitCode", raw), "coerce"); record("A", "B", withKey(E, "auditRounds", raw), "coerce"); }
// work-phase binding
for (const a of ["null", obj({ from: "P", to: "A", did: "x" }), obj({ from: "B", to: "C", did: "x", workPhaseId: "wp9" }), obj({ from: "B", to: "C", did: "x", workPhaseId: " wp2 " }), obj({ from: "B", to: "C", did: "x", workPhaseId: "wp2" }), obj({ from: "B", to: "C", did: "x", workPhaseId: "" }), obj({ from: "B", to: "C", did: "x", workPhaseId: 5 }), "[]"]) for (const active of [null, "wp2", ""]) {
  const att = A.coerceAttest(JSON.parse(a));
  bindings.push({ input: a, active, result: verdict(A.validateWorkPhaseBinding(att, active)) });
}
const lines = (key, list) => JSON.stringify(key) + ":[\n" + list.map((x) => JSON.stringify(x)).join(",\n") + "\n]";
writeFileSync(join(outDir, "oracle-attest.json"), "{" + [lines("texts", texts), lines("attest", attest), lines("tails", tails), lines("bindings", bindings)].join(",\n") + "}\n");

// plan gate: each case builds a tree under <root>/ws and names the unit with placeholders
const plan = [];
let m = 0;
const add = (id, tree, input, o = {}) => {
  const root = mkdtempSync(join(workRoot, "r"));
  const cwd = join(root, "ws");
  mkdirSync(cwd, { recursive: true });
  const expand = (s) => s.replaceAll("${CWD}", cwd).replaceAll("${ROOT}", root);
  const unexpand = (s) => s.replaceAll(cwd, "${CWD}").replaceAll(root, "${ROOT}");
  const unreadable = [];
  for (const e of tree) {
    const p = join(root, e.path);
    if (e.type === "dir") mkdirSync(p, { recursive: true });
    else if (e.type === "file") { mkdirSync(join(p, ".."), { recursive: true }); writeFileSync(p, "x"); }
    else if (e.type === "symlink") { mkdirSync(join(p, ".."), { recursive: true }); symlinkSync(expand(e.target), p); }
    else if (e.type === "chmod") unreadable.push(p);
  }
  for (const p of unreadable) chmodSync(p, 0);
  const raw = expand(input);
  const att = o.direct ? JSON.parse(raw) : A.coerceAttest(JSON.parse(raw));
  const r = G.validatePlanArtifacts(att, cwd);
  for (const p of unreadable) chmodSync(p, 0o755);
  plan.push({ id: "p" + m++ + "_" + id, direct: !!o.direct, tree, input, result: { ok: r.ok, ...(r.unit !== undefined ? { unit: unexpand(r.unit) } : {}), ...(r.reason !== undefined ? { reason: unexpand(r.reason) } : {}) } });
  rmSync(root, { recursive: true, force: true });
};
const A0 = (u, extra = {}) => obj({ from: "P", to: "A", did: "x", planUnit: u, ...extra });
const doc = (dir, name = "000_plan.md") => ({ path: "ws/" + dir + "/" + name, type: "file" });
add("null", [], "null"); add("no_planunit", [], obj({ from: "P", to: "A", did: "x" })); add("empty_planunit", [], A0("")); add("blank_planunit", [], A0("  "));
add("missing_unit", [], A0("devlog/_plan/000000_none"));
add("file_not_dir", [{ path: "ws/unit", type: "file" }], A0("unit"));
add("empty_dir", [{ path: "ws/unit", type: "dir" }], A0("unit"));
add("bare_name", [doc("unit", "PLAN.md")], A0("unit"));
for (const name of ["000_plan.md", "010_phase1.md", "000_.md", "000_x.md", "0000_x.md", "00_x.md", "000x.md", "000_x.MD", "000_x.txt", "000_x.md.bak", "000_.md.md", "000_a\rb.md", "000_a\nb.md", "000_a\u2028b.md", "000_a\u2029b.md", "000_ .md", "\u0660\u0660\u0660_x.md", "000_\u00e9.md", "999_z.md", "a000_x.md", ".000_x.md", "000_x.md "]) add("doc_" + JSON.stringify(name).slice(1, -1).replace(/[^A-Za-z0-9_.-]/g, "_"), [doc("unit", name)], A0("unit"));
add("dir_named_like_doc", [{ path: "ws/unit/000_a.md", type: "dir" }], A0("unit"));
add("nested_doc_only", [doc("unit/sub")], A0("unit"));
add("symlinked_unit", [doc("real"), { path: "ws/unit", type: "symlink", target: "${CWD}/real" }], A0("unit"));
add("dangling_symlink_doc", [{ path: "ws/unit", type: "dir" }, { path: "ws/unit/000_x.md", type: "symlink", target: "${CWD}/nowhere" }], A0("unit"));
add("symlink_doc_to_file", [{ path: "ws/elsewhere.txt", type: "file" }, { path: "ws/unit/000_x.md", type: "symlink", target: "${CWD}/elsewhere.txt" }], A0("unit"));
add("absolute_unit", [doc("unit")], A0("${CWD}/unit"));
add("absolute_trailing_slash", [doc("unit")], A0("${CWD}/unit/"));
add("absolute_dotdot", [doc("unit"), doc("x")], A0("${CWD}/x/../unit"));
add("absolute_outside_cwd", [{ path: "other/000_o.md", type: "file" }], A0("${ROOT}/other"));
add("relative_dotslash", [doc("a/b")], A0("./a//b/"));
add("relative_dotdot_inside", [doc("a/b"), doc("a/c")], A0("a/../a/b"));
add("relative_dotdot_sibling", [{ path: "other/dir/000_o.md", type: "file" }], A0("../other/dir"));
add("relative_dotdot_missing", [], A0("../../x"));
add("unit_dot", [{ path: "ws/000_a.md", type: "file" }], A0("."));
add("unit_dot_no_doc", [], A0("."));
add("unit_dot_slash", [{ path: "ws/000_a.md", type: "file" }], A0("./"));
add("nul_in_unit", [], A0("a\u0000b"));
add("trimmed_unit", [doc("unit")], A0("  unit  "));
add("planpaths_relative_ok", [doc("unit", "000_plan.md"), doc("unit", "010_p.md")], A0("unit", { planPaths: ["unit/010_p.md"] }));
add("planpaths_absolute_ok", [doc("unit")], A0("unit", { planPaths: ["${CWD}/unit/000_plan.md"] }));
add("planpaths_missing", [doc("unit")], A0("unit", { planPaths: ["unit/999_ghost.md"] }));
add("planpaths_first_missing_second_ok", [doc("unit")], A0("unit", { planPaths: ["unit/ghost.md", "unit/000_plan.md"] }));
add("planpaths_outside_unit", [doc("unit"), { path: "ws/other.txt", type: "file" }], A0("unit", { planPaths: ["other.txt", "${ROOT}", "/etc"] }));
add("planpaths_dangling_symlink", [doc("unit"), { path: "ws/dl", type: "symlink", target: "${CWD}/nowhere" }], A0("unit", { planPaths: ["dl"] }));
add("planpaths_direct_empty_entry", [doc("unit")], A0("unit", { planPaths: [""] }), { direct: true });
add("planpaths_coerced_empty_entry", [doc("unit")], A0("unit", { planPaths: ["", " ", 4] }));
add("planpaths_nul", [doc("unit")], A0("unit", { planPaths: ["a\u0000b"] }));
add("direct_untrimmed_unit", [doc("unit")], A0(" unit "), { direct: true });
add("unreadable_unit", [doc("unit"), { path: "ws/unit", type: "chmod" }], A0("unit"));
add("unreadable_unit_with_planpaths", [doc("unit"), { path: "ws/unit", type: "chmod" }], A0("unit", { planPaths: ["unit/000_plan.md"] }));
add("unit_is_cwd_parent", [{ path: "000_a.md", type: "file" }], A0(".."));
add("trailing_dotdot", [doc("unit")], A0("unit/sub/.."));
add("long_name", [doc("unit", "000_" + "a".repeat(200) + ".md")], A0("unit"));
add("unicode_unit_name", [doc("d\u00e9j\u00e0")], A0("d\u00e9j\u00e0"));
add("space_in_unit_name", [doc("my unit")], A0("my unit"));
add("backslash_in_unit", [doc("a\\b")], A0("a\\b"));
add("tilde_unit", [doc("~")], A0("~"));
writeFileSync(join(outDir, "oracle-plan-gate.json"), "{" + lines("plan", plan) + "}\n");
console.log(JSON.stringify({ attest: attest.length, tails: tails.length, bindings: bindings.length, plan: plan.length }));
