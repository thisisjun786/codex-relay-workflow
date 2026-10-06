// The oracle worker for the goalplan target. The harness's worker pool starts it as
// `node shim.mjs` and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer                   ->  {"id":1,"error":{"name":...,"message":...}}
//
// The oracle is CXC v0.2.40's readGoalplanDetailed and writeGoalplan
// (plugins/codexclaw/components/pabcd-state/dist/goalplan.js:698, :921), imported under ORACLE_ROOT
// the way the record-oracle.mjs recorders do. The case's plan bytes were already written under this
// root's .codexclaw/goalplans/rec-plan/goalplan.json by the harness's fs scenario; the shim reads it,
// rewrites it, and answers the read kind, field and plan text, and the rewritten bytes.
import { createInterface } from "node:readline";
import { readFileSync } from "node:fs";
import { join } from "node:path";

// The harness always sets ORACLE_ROOT to the target's Oracle.Root; no host path is committed here.
const oracleRoot = process.env.ORACLE_ROOT;
if (!oracleRoot) throw new Error("ORACLE_ROOT is not set");
const { readGoalplanDetailed, writeGoalplan, goalplanDir, GOALPLAN_FILE } = await import(oracleRoot + "/pabcd-state/dist/goalplan.js");

// goalplanPath is module-private in the oracle, so the shim rebuilds it from the exported
// goalplanDir and GOALPLAN_FILE.
const goalplanPath = (cwd, slug) => join(goalplanDir(cwd, slug), GOALPLAN_FILE);

const SLUG = "rec-plan";

// Both sides stamp the wall clock into updatedAt, so the two values can never match; each is rewritten
// to one placeholder before the answers compare. The Go side masks its own the same way.
const TIMESTAMP = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z/g;
const TIMESTAMP_PLACEHOLDER = "@TS@";

function mask(text) {
  return text.replace(TIMESTAMP, TIMESTAMP_PLACEHOLDER);
}

// run puts the homes the case declared under its own root, so a shim never reads a real one, then
// reads, rewrites and re-reads the plan the way the port does.
function run(request) {
  const root = typeof request.root === "string" ? request.root : "";
  if (root !== "") {
    process.env.HOME = root + "/home";
    process.env.CODEX_HOME = root + "/codex-home";
    process.env.CRW_HOME = root + "/crw-home";
    process.env.CODEXCLAW_HOME = root + "/codexclaw-home";
    process.env.TMPDIR = root + "/tmp";
  }
  const read = readGoalplanDetailed(root, SLUG);
  const answer = { kind: "ok" };
  if (read.diagnostic) {
    answer.kind = read.diagnostic.kind;
    if (typeof read.diagnostic.field === "string" && read.diagnostic.field !== "") {
      answer.field = read.diagnostic.field;
    }
  }
  answer.plan = read.plan === null ? null : mask(JSON.stringify(read.plan, null, 2));
  if (read.plan === null) return answer;
  try {
    writeGoalplan(root, read.plan);
  } catch (error) {
    answer.writeError = error instanceof Error ? error.message : String(error);
    return answer;
  }
  try {
    answer.written = mask(readFileSync(goalplanPath(root, SLUG), "utf8"));
  } catch (error) {
    answer.writeError = error instanceof Error ? error.message : String(error);
  }
  return answer;
}

const lines = createInterface({ input: process.stdin, terminal: false });
lines.on("line", (line) => {
  const text = line.trim();
  if (text === "") return;
  let request;
  try {
    request = JSON.parse(text);
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: null, error: { name: error.name, message: error.message } }) + "\n");
    return;
  }
  try {
    process.stdout.write(JSON.stringify({ id: request.id, output: run(request) }) + "\n");
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: request.id, error: { name: error.name, message: error.message } }) + "\n");
  }
});
lines.on("close", () => process.exit(0));
