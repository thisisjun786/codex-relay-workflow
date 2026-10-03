// Records the CXC v0.2.40 oracle's answers for the freeze units (freeze.ts, freeze-cli.ts) into oracle-freeze.json; the Go
// tests replay them (no Node at test time). Recorded with Node v24.20.0 (ICU 78.3, locale en-US) as
//   node record-freeze-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> > oracle-freeze.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d). A scenario names its files
// relative to the state directory (.codexclaw here, .crw in Go); its steps edit and run the command, and the tree it leaves is listed
// with the state directory written as the placeholder STATE in braces after a dollar sign.
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, symlinkSync, readdirSync, readFileSync, lstatSync } from "node:fs";
import { dirname, join } from "node:path";

const [dist, work] = process.argv.slice(2);
const freeze = await import(dist + "/freeze.js");
const cli = await import(dist + "/freeze-cli.js");
const sign = (n) => (n < 0 ? "<" : n > 0 ? ">" : "=");
const out = {};
const WS = "$" + "{WS}", STATE = "$" + "{STATE}";

// collation: single characters, a pairwise matrix and sorted path sets, all through localeCompare as computePlanHash does.
const chars = [];
for (let c = 1; c < 128; c++) chars.push(String.fromCharCode(c));
out.asciiOrder = [...chars].sort((a, b) => a.localeCompare(b)).join("");
const matrix = ["a", "A", "b", "B", "_", "-", ".", "/", "0", "9", "10", "a.md", "A.md", "a_b.md", "aB.md", "a-b.md", "ab.md", "a b.md", "a/b.md", "a.b", "000_plan.md", "010_phase.md", "000-plan.md", "Z.md", "z.md", "~", "$", "\t", "", "a\u0001b", "ab"];
out.matrix = { strings: matrix, rows: matrix.map((a) => matrix.map((b) => sign(a.localeCompare(b))).join("")) };
const sets = [
  ["b.md", "a.md", "B.md", "A.md", "a_b.md", "aB.md", "a-b.md", "ab.md", "a b.md", "a.b", "_x.md", "-y.md", "10.md", "9.md", "sub/x.md", "sub.md", "sub-x.md", "Sub/y.md"],
  ["000_plan.md", "010_phase.md", "000-plan.md", "000_Plan.md", "0.md", "00.md", "references/a.md", "references/B.md", "references.md"],
  ["z", "Z", "y", "Y", "~", "$", "^", "`", "[", "]", "{", "}", "|", "\\", "<", ">", "=", "+", "&", "%", "#", "@", "*", "!", "?", ",", ";", ":", "'", "\"", "(", ")"],
  ["010_계획.md", "000_plan.md", "a.md", "Z.md", "가.md"],
];
out.sets = sets.map((paths) => {
  const files = paths.map((path) => ({ path, sha256: freeze.sha256(path + "!") }));
  const m = freeze.buildFreezeManifest({ objective: "o", planFiles: files, evidenceBundle: {}, now: () => "T" });
  return { files, sorted: m.planFiles.map((f) => f.path), planHash: m.planHash, hash: freeze.computePlanHash(files) };
});

// slugs
const objectives = ["", "  Hello, World!!  ", "a".repeat(60), "a".repeat(47) + "-b", "a".repeat(47) + "--b", "***", "\u0130stanbul Plan", "Kelvin \u212a plan", "한글 only", "Mixed 한글 abc", "-----x", "UPPER_snake-CASE 99", "tab\tnew\nline", "\u00df\u03a3", "\u0130", "x\u0130y", "Build the Thing!", "a--b", "1 2 3"];
out.slugs = objectives.map((objective) => ({ objective, slug: freeze.deriveSlug(objective) }));

// stale checks
const h = (path, c) => ({ path, sha256: freeze.sha256(c) });
const base = [h("a.md", "1"), h("b.md", "2"), h("sub/c.md", "3")];
const stale = (name, files, current, tamper) => {
  const m = freeze.buildFreezeManifest({ objective: "o", planFiles: files, evidenceBundle: {}, now: () => "T" });
  if (tamper) m.planHash = tamper;
  return { name, manifest: m, current, result: freeze.checkStale(m, current) };
};
out.stale = [
  stale("same", base, base),
  stale("changed", base, [h("a.md", "1"), h("b.md", "X"), h("sub/c.md", "3")]),
  stale("missing", base, [h("a.md", "1")]),
  stale("new", base, [...base, h("new.md", "9")]),
  stale("all_three", base, [h("b.md", "X"), h("z.md", "9")]),
  stale("hash_only", base, base, "0".repeat(64)),
  stale("empty_both", [], []),
  stale("utf16_order", [h("a.md", "1")], [h("\ue000.md", "1"), h("\ud83d\ude00.md", "1"), h("a.md", "2")]),
];

// arguments
const argvs = [[], ["help"], ["--help"], ["-h"], ["--cwd"], ["--cwd", "/x"], ["--session", "a", "--dry-run"], ["--session", "--dry-run"], ["--session"], ["--dry-run"], ["--cwd", "--help"], ["--session", "help"], ["--cwd", "a", "--cwd", "b"], ["x"], ["--cwd=/x"], ["--session", "s", "--cwd", "d", "extra"]];
out.argv = argvs.map((argv) => {
  const a = cli.parseFreezeArgs(argv);
  return { argv, cwd: a.cwd === process.cwd() ? null : a.cwd, sessionId: a.sessionId, dryRun: a.dryRun, help: a.help ?? null };
});

// scenarios
const score = (level) => ({ level, known: ["k"], unknown: [], confidence: 1 });
const sessionFile = (o) => ({ phase: "I", slug: "demo", flags: { interview: true, auditPassed: false, checkPassed: false }, orchestrationActive: false, lastInjectedPhase: null, injectedTurns: [], stopBlockPhase: null, stopBlockCount: 0, ...o });
const ready = { dimensions: { goal: score("max"), constraint: score("max"), success: score("max"), ontology: score("max") }, contradictions: [], assumptions: [{ text: "Assume X", recorded: true }], scanRounds: 1 };
const rich = { dimensions: { goal: score("high"), constraint: { level: "mid", known: ["a", "b"], unknown: ["c"], confidence: 0.3333333333333333 }, success: score("max"), ontology: { level: "low", known: [], unknown: [], confidence: 0.5 } }, contradictions: [{ contradictionId: "c1", severity: "high", summary: "A <b> & \u2028 ü" }], assumptions: [{ text: "A <1>", recorded: true }, { text: "B", recorded: false }, { text: "C\u2029", recorded: true }], scanRounds: 2 };
const t = (path, text) => ({ path, text });
const b = (path, bytes) => ({ path, b64: Buffer.from(bytes).toString("base64") });
const plan = [t("plan/demo/000_plan.md", "# plan\n"), t("plan/demo/010_phase.md", "# phase\n")];
const session = (id, o) => t("sessions/" + id + ".json", JSON.stringify(sessionFile(o)));
const prior = (text) => ({ files: [...plan, session("s1", { interview: null }), t("interview/freeze.json", text)], steps: [{ session: "s1", dryRun: true }] });
const good = { planFiles: [{ path: "000_plan.md", sha256: freeze.sha256("# plan\n") }, { path: "010_phase.md", sha256: freeze.sha256("# phase\n") }] };
good.planHash = freeze.computePlanHash(good.planFiles);
const named = (names) => names.map((n) => t("plan/default/" + n, n));
const scenarios = {
  empty_default: { files: [], steps: [{ dryRun: false }, { dryRun: false }] },
  dry_run_absent_dir: { files: [], steps: [{ dryRun: true }] },
  nested_hidden: { files: [...plan, t("plan/demo/sub/020_x.md", "x"), t("plan/demo/sub/deep/030.md", "y"), t("plan/demo/.hidden.md", "h"), t("plan/demo/.hd/z.md", "z"), t("plan/demo/sub/.h", "q"), { path: "plan/demo/empty", dir: true }, session("s1", { interview: null })], steps: [{ session: "s1", dryRun: false }] },
  case_and_punct: { files: named(["a.md", "B.md", "_x.md", "-y.md", "a_b.md", "aB.md", "a-b.md", "10.md", "9.md", "A.md", "a b.md", "Z.md", "sub/x.md", "sub.md"]), steps: [{ dryRun: false }] },
  invalid_utf8_and_bom: { files: [b("plan/default/bad.md", [0x61, 0xff, 0xfe, 0x62]), b("plan/default/trunc.md", [0xe2, 0x82]), b("plan/default/bom.md", [0xef, 0xbb, 0xbf, 0x78]), b("plan/default/surr.md", [0xed, 0xa0, 0x80]), b("plan/default/ok.md", [0x6f, 0x6b])], steps: [{ dryRun: false }] },
  hangul_names: { files: named(["010_계획.md", "000_plan.md", "a.md", "가.md"]), steps: [{ dryRun: false }] },
  symlinks: { files: [...plan, { path: "plan/demo/link.md", link: "000_plan.md" }, { path: "plan/demo/linkdir", link: "sub" }, t("plan/demo/sub/x.md", "x"), session("s1", { interview: null })], steps: [{ session: "s1", dryRun: false }] },
  broken_symlink: { files: [...plan, { path: "plan/demo/gone.md", link: "nowhere" }, session("s1", { interview: null })], steps: [{ session: "s1", dryRun: false }] },
  stale_changed: { files: [...plan, session("s1", { interview: null })], steps: [{ session: "s1", dryRun: false }, { session: "s1", dryRun: true }, { session: "s1", dryRun: true, edits: [t("plan/demo/000_plan.md", "# changed\n"), t("plan/demo/new.md", "n"), { path: "plan/demo/010_phase.md", remove: true }] }, { session: "s1", dryRun: false }, { session: "s1", dryRun: true }] },
  ready_interview: { files: [...plan, session("s1", { interview: ready })], steps: [{ session: "s1", dryRun: true }] },
  rich_tracker: { files: [...plan, session("s1", { interview: rich })], steps: [{ session: "s1", dryRun: false }] },
  session_id_objective: { files: [t("plan/my-session/000_plan.md", "p")], steps: [{ session: "My Session!", dryRun: false }] },
  state_slug_wins: { files: [t("plan/other-name/000_plan.md", "p"), session("s1", { slug: "Other Name!", interview: null })], steps: [{ session: "s1", dryRun: true }] },
  interview_is_file: { files: [t("interview", "x")], steps: [{ dryRun: false }] },
  manifest_is_dir: { files: [...plan, session("s1", { interview: null }), t("interview/freeze.json/keep", "k")], steps: [{ session: "s1", dryRun: true }, { session: "s1", dryRun: false }] },
  prior_garbage: prior("{not json"),
  prior_null: prior("null"),
  prior_array: prior("[]"),
  prior_number: prior("42"),
  prior_empty_object: prior("{}"),
  prior_plan_files_string: prior('{"planFiles":"abc"}'),
  prior_null_entry: prior('{"planFiles":[null]}'),
  prior_empty_files: prior('{"planFiles":[]}'),
  prior_no_hash: prior(JSON.stringify({ planFiles: good.planFiles })),
  prior_good: prior(JSON.stringify(good)),
  prior_good_pretty_extra: prior(JSON.stringify({ ...good, extra: 1, objective: "demo" }, null, 2)),
};
const apply = (ws, f) => {
  const p = join(ws, ".codexclaw", f.path);
  if (f.remove) return rmSync(p, { recursive: true, force: true });
  if (f.dir) return mkdirSync(p, { recursive: true });
  mkdirSync(dirname(p), { recursive: true });
  if (f.link !== undefined) symlinkSync(f.link, p);
  else writeFileSync(p, f.b64 !== undefined ? Buffer.from(f.b64, "base64") : f.text);
};
const list = (root, rel = "") => readdirSync(join(root, rel)).sort().flatMap((n) => {
  const r = rel ? rel + "/" + n : n;
  return lstatSync(join(root, r)).isDirectory() ? [r + "/", ...list(root, r)] : [r];
});
out.scenarios = {};
for (const [id, sc] of Object.entries(scenarios)) {
  const ws = mkdtempSync(join(work, "f-"));
  for (const f of sc.files) apply(ws, f);
  const steps = [];
  for (const step of sc.steps) {
    for (const f of step.edits ?? []) apply(ws, f);
    let output = null, error = false;
    try {
      output = cli.runFreeze({ cwd: ws, sessionId: step.session ?? "default", dryRun: step.dryRun }).split(ws).join(WS);
    } catch { error = true; }
    let manifest = null;
    try { manifest = readFileSync(join(ws, ".codexclaw/interview/freeze.json"), "utf8").replace(/"frozenAt": "[^"]*"/, '"frozenAt": "<TS>"'); } catch {}
    steps.push({ session: step.session ?? "default", dryRun: step.dryRun, edits: step.edits ?? [], output, error, manifest, tree: list(ws).map((p) => p.replace(/^\.codexclaw/, STATE)) });
  }
  out.scenarios[id] = { files: sc.files, steps };
  rmSync(ws, { recursive: true, force: true });
}
process.stdout.write(JSON.stringify(out, null, 1) + "\n");
