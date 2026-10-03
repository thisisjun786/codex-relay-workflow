// Records the answers of the CXC v0.2.40 oracle's metrics.ts for the scenarios of scenarios-metrics.json; the Go tests replay
// them from oracle-metrics.json (no Node at test time). Recorded with Node v24.20.0 as
//   node record-metrics-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist scenarios-metrics.json <short work dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d) with its dist/ built.
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

const [oracleDist, scenarioFile, workRoot] = process.argv.slice(2);
const oracle = await import(oracleDist + "/metrics.js");
const tokens = { NaN, Infinity, "-Infinity": -Infinity, "-0": -0 };
const arg = (v) => (typeof v === "string" && v in tokens ? tokens[v] : v);
// -0, NaN and the infinities are answered as strings; JSON cannot hold them.
const spell = (x) => (typeof x === "number" && (!Number.isFinite(x) || Object.is(x, -0)) ? (Object.is(x, -0) ? "-0" : String(x)) : x);
const shown = (v) => JSON.parse(JSON.stringify(v, (_, x) => spell(x)));

function step(cwd, o) {
  const state = join(cwd, ".codexclaw");
  const at = (path) => join(state, path);
  const clock = () => {
    if (!o.now) throw new Error("scenario op without now");
    return { now: () => o.now };
  };
  const phase = o.workPhase === undefined ? {} : { workPhaseId: o.workPhase };
  switch (o.op) {
    case "record":
      return oracle.recordObjectiveMetric(cwd, { sessionId: o.session, metricName: o.name, value: arg(o.value), source: o.source ?? "operator-entered", ...phase, ...clock() });
    case "ingest":
      return { rows: oracle.recordMetricsFromText(cwd, { sessionId: o.session, text: o.text, source: o.source ?? "evaluate.sh", ...phase, ...clock() }) };
    case "read":
      return { rows: oracle.readObjectiveMetrics(cwd, o.session) };
    case "kind":
      if (o.set) oracle.writeObjectiveKind(cwd, o.session, o.set);
      return { kind: oracle.readObjectiveKind(cwd, o.session), explicit: oracle.readExplicitObjectiveKind(cwd, o.session) };
    case "plateau":
      return oracle.checkObjectivePlateau(cwd, o.session, { minRecords: arg(o.minRecords), noiseFloor: arg(o.noiseFloor) });
    case "parse":
      return { parsed: oracle.parseMetricLine(o.line) };
    case "file":
      mkdirSync(dirname(at(o.path)), { recursive: true });
      writeFileSync(at(o.path), o.b64 === undefined ? o.text : Buffer.from(o.b64, "base64"));
      return null;
    case "dir":
      mkdirSync(at(o.path), { recursive: true });
      return null;
  }
  throw new Error("unknown op " + o.op);
}

// The files the oracle leaves under .codexclaw, by relative path; the temp file of a kind write must be gone, updatedAt is a placeholder.
function files(dir, rel = "") {
  const out = {};
  for (const e of readdirSync(join(dir, rel), { withFileTypes: true })) {
    const path = rel ? rel + "/" + e.name : e.name;
    if (e.isDirectory()) Object.assign(out, files(dir, path));
    else if (e.name !== ".gitignore") out[path] = readFileSync(join(dir, path), "utf8").replace(/("updatedAt": ")\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z"/, '$1<TS>"');
  }
  return out;
}

const out = {};
for (const sc of JSON.parse(readFileSync(scenarioFile, "utf8"))) {
  const cwd = mkdtempSync(join(workRoot, "s-"));
  const results = sc.ops.map((o) => {
    try {
      return shown(step(cwd, o) ?? null);
    } catch (err) {
      return { error: true };
    }
  });
  out[sc.id] = { results, ...(sc.capture ? { files: files(join(cwd, ".codexclaw")) } : {}) };
  rmSync(cwd, { recursive: true, force: true });
}
// One line per answer, written as ASCII so no editor or JSON reader is asked to carry U+2028, a BOM or a lone surrogate.
const ascii = (s) => s.replace(/[\u007f-\uffff]/g, (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));
const body = Object.entries(out).map(([id, { results, files }]) => {
  const rows = results.map((r) => "   " + ascii(JSON.stringify(r))).join(",\n");
  return " " + JSON.stringify(id) + ": {\n  \"results\": [\n" + rows + "\n  ]" + (files ? ",\n  \"files\": " + ascii(JSON.stringify(files)) : "") + "\n }";
});
process.stdout.write("{\n" + body.join(",\n") + "\n}\n");
