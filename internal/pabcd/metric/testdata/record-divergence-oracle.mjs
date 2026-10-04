// Records the answers of the CXC v0.2.40 oracle's divergence.ts for the scenarios of scenarios-divergence.json; the Go tests replay
// them from oracle-divergence.json (no Node at test time). Recorded with Node v24.20.0 as
//   node record-divergence-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist scenarios-divergence.json <short work dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d) with its dist/ built.
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

const [oracleDist, scenarioFile, workRoot] = process.argv.slice(2);
const oracle = await import(oracleDist + "/divergence.js");
const tokens = { NaN, Infinity, "-Infinity": -Infinity, "-0": -0 };
const arg = (v) => (typeof v === "string" && v in tokens ? tokens[v] : v);
// A result is the text JSON.stringify gives the value, so key order and spelling are compared (U+2028, HTML and -0 as it spells them).
// The answer file itself is ASCII, so no editor or JSON reader is asked to carry U+2028, a BOM or a lone surrogate.
const ascii = (s) => s.replace(/[\u007f-\uffff]/g, (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));
const text = (v) => JSON.stringify(v ?? null);

function step(cwd, o) {
  const at = (path) => join(cwd, ".codexclaw", path);
  const now = o.now === undefined ? undefined : () => o.now;
  switch (o.op) {
    case "mode":
      return oracle.writeDivergenceMode(cwd, { sessionId: o.session, active: o.active, collapsePoint: o.collapse, reason: o.reason, now });
    case "mode-read":
      return oracle.readDivergenceMode(cwd, o.session);
    case "cand":
      return oracle.recordDivergenceCandidate(cwd, {
        sessionId: o.session, id: o.id, kind: o.kind, title: o.title, rationale: o.rationale, sourceUrls: o.urls, status: o.status,
        worktree: o.worktree, metricName: o.metricName, metricValue: arg(o.metricValue), note: o.note, changeClass: o.changeClass,
        killedAtPhase: o.killedAtPhase, now,
      });
    case "cand-read":
      return oracle.readDivergenceCandidates(cwd, o.session);
    case "streak":
      return oracle.discardStreak(o.rows);
    case "file":
      mkdirSync(dirname(at(o.path)), { recursive: true });
      writeFileSync(at(o.path), o.b64 === undefined ? o.text : Buffer.from(o.b64, "base64"));
      return null;
    case "symlink":
      mkdirSync(dirname(at(o.path)), { recursive: true });
      symlinkSync(o.target, at(o.path));
      return null;
    case "dir":
      mkdirSync(at(o.path), { recursive: true });
      return null;
  }
  throw new Error("unknown op " + o.op);
}

// The files left under .codexclaw, by relative path (a temp file of a mode write must be gone).
const files = (dir, rel = "") =>
  readdirSync(join(dir, rel), { withFileTypes: true }).reduce((out, e) => {
    const path = rel ? rel + "/" + e.name : e.name;
    if (e.isDirectory()) return { ...out, ...files(dir, path) };
    if (e.name === ".gitignore") return out;
    return { ...out, [path]: readFileSync(join(dir, path), "utf8") };
  }, {});

const lines = JSON.parse(readFileSync(scenarioFile, "utf8")).map((sc) => {
  const cwd = mkdtempSync(join(workRoot, "s-"));
  const results = sc.ops.map((o) => {
    try {
      return text(step(cwd, o));
    } catch (err) {
      // The texts the CLI prints are compared; a filesystem text differs between Node and Go.
      return text({ error: String(err.message).startsWith("divergence candidate") ? err.message : true });
    }
  });
  const answer = { results, ...(sc.capture ? { files: files(join(cwd, ".codexclaw")) } : {}) };
  rmSync(cwd, { recursive: true, force: true });
  return " " + JSON.stringify(sc.id) + ": " + ascii(JSON.stringify(answer));
});
process.stdout.write("{\n" + lines.join(",\n") + "\n}\n");
