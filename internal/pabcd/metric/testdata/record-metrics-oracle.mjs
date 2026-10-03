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
// -0, NaN and the infinities are answered as strings; JSON cannot hold them. Everything is written as ASCII, so no editor or JSON
// reader is asked to carry U+2028, a BOM or a lone surrogate.
const spell = (_, x) => (typeof x === "number" && (!Number.isFinite(x) || Object.is(x, -0)) ? (Object.is(x, -0) ? "-0" : String(x)) : x);
const json = (v) => JSON.stringify(v, spell).replace(/[\u007f-\uffff]/g, (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));

function step(cwd, o) {
  const at = (path) => join(cwd, ".codexclaw", path);
  if ((o.op === "record" || o.op === "ingest") && !o.now) throw new Error("scenario op without now");
  const opts = { sessionId: o.session, now: () => o.now, ...(o.workPhase === undefined ? {} : { workPhaseId: o.workPhase }) };
  switch (o.op) {
    case "record":
      return oracle.recordObjectiveMetric(cwd, { ...opts, metricName: o.name, value: arg(o.value), source: o.source ?? "operator-entered" });
    case "ingest":
      return { rows: oracle.recordMetricsFromText(cwd, { ...opts, text: o.text, source: o.source ?? "evaluate.sh" }) };
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

// The files left under .codexclaw, by relative path, with updatedAt a placeholder (the temp file of a kind write must be gone).
const files = (dir, rel = "") =>
  readdirSync(join(dir, rel), { withFileTypes: true }).reduce((out, e) => {
    const path = rel ? rel + "/" + e.name : e.name;
    if (e.isDirectory()) return { ...out, ...files(dir, path) };
    if (e.name === ".gitignore") return out;
    return { ...out, [path]: readFileSync(join(dir, path), "utf8").replace(/("updatedAt": ")\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z"/, '$1<TS>"') };
  }, {});

const lines = JSON.parse(readFileSync(scenarioFile, "utf8")).map((sc) => {
  const cwd = mkdtempSync(join(workRoot, "s-"));
  const results = sc.ops.map((o) => {
    try {
      return JSON.parse(json(step(cwd, o) ?? null));
    } catch (err) {
      return { error: true };
    }
  });
  const answer = { results, ...(sc.capture ? { files: files(join(cwd, ".codexclaw")) } : {}) };
  rmSync(cwd, { recursive: true, force: true });
  return " " + JSON.stringify(sc.id) + ": " + json(answer);
});
process.stdout.write("{\n" + lines.join(",\n") + "\n}\n");
