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
import { copyFileSync, existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// The harness always sets ORACLE_ROOT to the target's Oracle.Root; no host path is committed here.
const oracleRoot = process.env.ORACLE_ROOT;
if (!oracleRoot) throw new Error("ORACLE_ROOT is not set");
const { readGoalplanDetailed, writeGoalplan, goalplanDir, GOALPLAN_FILE } = await import(oracleRoot + "/pabcd-state/dist/goalplan.js");

// goalplanPath is module-private in the oracle, so the shim rebuilds it from the exported
// goalplanDir and GOALPLAN_FILE.
const goalplanPath = (cwd, slug) => join(goalplanDir(cwd, slug), GOALPLAN_FILE);

const SLUG = "rec-plan";

// writeGoalplan restamps the plan's updatedAt from the wall clock, so the two sides' written values
// can never match; each is rewritten to one placeholder before the answers compare. Only that key is
// masked, and only at the plan's top level: every other timestamp-shaped value (a review round's
// openedAt, a persisted updatedAt, a sourceIdentity's capturedAt) is compared as stored, because
// masking by shape would hide a persisted timestamp the port rewrites to another instant (CRW-708
// generation 3, c8). The read form needs no mask: the oracle's reader defaults a missing or non-text
// updatedAt to the epoch, a fixed value both sides keep, and it never stamps the clock.
const TIMESTAMP = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;
const TIMESTAMP_PLACEHOLDER = "@TS@";

// topLevelUpdatedAtSpan is the [start, end) span of the string value of the plan's top-level
// "updatedAt", or null when it has none. It walks the text tracking string and container depth, so a
// nested "updatedAt" (a finalGate's) is never mistaken for the top-level one — the same span the Go
// side's token walk finds.
function topLevelUpdatedAtSpan(text) {
  let depth = 0;
  let i = 0;
  while (i < text.length) {
    const ch = text[i];
    if (ch === '"') {
      let j = i + 1;
      while (j < text.length && text[j] !== '"') {
        if (text[j] === "\\") j++;
        j++;
      }
      const raw = text.slice(i, j + 1);
      const colon = /^\s*:\s*/.exec(text.slice(j + 1));
      if (depth === 1 && colon !== null && JSON.parse(raw) === "updatedAt") {
        const start = j + 1 + colon[0].length;
        if (text[start] !== '"') return null;
        let k = start + 1;
        while (k < text.length && text[k] !== '"') {
          if (text[k] === "\\") k++;
          k++;
        }
        return [start, k + 1];
      }
      i = j + 1;
      continue;
    }
    if (ch === "{" || ch === "[") depth++;
    else if (ch === "}" || ch === "]") depth--;
    i++;
  }
  return null;
}

// maskUpdatedAt is the plan text with the value of its top-level updatedAt replaced by the
// placeholder, when that value is a wall-clock stamp. A parsed copy decides whether to mask; the
// replacement is spliced into the original text so every other byte is unchanged.
function maskUpdatedAt(text) {
  let parsed;
  try {
    parsed = JSON.parse(text);
  } catch {
    return text;
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return text;
  if (typeof parsed.updatedAt !== "string" || !TIMESTAMP.test(parsed.updatedAt)) return text;
  const span = topLevelUpdatedAtSpan(text);
  if (span === null) return text;
  return text.slice(0, span[0]) + '"' + TIMESTAMP_PLACEHOLDER + '"' + text.slice(span[1]);
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
  // The case stores the plan once, at the port's own path. The oracle reads .codexclaw/goalplans, so
  // the shim mirrors the file there first: the document is one value in the input, and the shrinker
  // can never leave the two sides reading different documents.
  const source = join(root, ".crw", "goalplans", SLUG, "goalplan.json");
  const target = join(root, ".codexclaw", "goalplans", SLUG, "goalplan.json");
  if (existsSync(source)) {
    mkdirSync(join(root, ".codexclaw", "goalplans", SLUG), { recursive: true });
    copyFileSync(source, target);
  }
  const read = readGoalplanDetailed(root, SLUG);
  const answer = { kind: "ok" };
  if (read.diagnostic) {
    answer.kind = read.diagnostic.kind;
    if (typeof read.diagnostic.field === "string" && read.diagnostic.field !== "") {
      answer.field = read.diagnostic.field;
    }
  }
  answer.plan = read.plan === null ? null : JSON.stringify(read.plan, null, 2);
  if (read.plan === null) return answer;
  try {
    writeGoalplan(root, read.plan);
  } catch (error) {
    answer.writeError = error instanceof Error ? error.message : String(error);
    return answer;
  }
  try {
    answer.written = maskUpdatedAt(readFileSync(goalplanPath(root, SLUG), "utf8"));
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
