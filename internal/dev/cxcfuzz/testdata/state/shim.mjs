// The oracle worker for the state target. The harness's worker pool starts it as
// `node shim.mjs` and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer                   ->  {"id":1,"error":{"name":...,"message":...}}
//
// The oracle is CXC v0.2.40's readStateStrict and writeState
// (plugins/codexclaw/components/pabcd-state/dist/state.js:494, :620), imported under ORACLE_ROOT the
// way the record-oracle.mjs recorders do. The case's session bytes were already written under this
// root's .codexclaw/sessions/s.json by the harness's fs scenario; the shim reads it, rewrites it,
// and answers the unreadable verdict, the rebuilt state and the rewritten bytes, or the refusal.
import { createInterface } from "node:readline";
import { copyFileSync, existsSync, mkdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";

// The harness always sets ORACLE_ROOT to the target's Oracle.Root; no host path is committed here.
const oracleRoot = process.env.ORACLE_ROOT;
if (!oracleRoot) throw new Error("ORACLE_ROOT is not set");
const { readStateStrict, writeState, statePath } = await import(oracleRoot + "/pabcd-state/dist/state.js");

const SESSION_ID = "s";

// Both sides stamp the wall clock into updatedAt, so the two values can never match. Each is rewritten
// to one placeholder before the answers compare; the Go side masks its own the same way.
const TIMESTAMP = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z/g;

// The Go side writes the same literal; it holds no regexp metacharacter.
const TIMESTAMP_PLACEHOLDER = "@TS@";

function mask(text) {
  return text.replace(TIMESTAMP, TIMESTAMP_PLACEHOLDER);
}

// run puts the homes the case declared under its own root, so a shim never reads a real one, then
// reads, rewrites and re-reads the session file the way the port does.
function run(request) {
  const root = typeof request.root === "string" ? request.root : "";
  if (root !== "") {
    process.env.HOME = root + "/home";
    process.env.CODEX_HOME = root + "/codex-home";
    process.env.CRW_HOME = root + "/crw-home";
    process.env.CODEXCLAW_HOME = root + "/codexclaw-home";
    process.env.TMPDIR = root + "/tmp";
  }
  // The case stores the session document once, at the port's own path. The oracle reads
  // .codexclaw/sessions, so the shim mirrors the file there first: the document is one value in the
  // input, and the shrinker can never leave the two sides reading different documents.
  const source = join(root, ".crw", "sessions", SESSION_ID + ".json");
  const target = join(root, ".codexclaw", "sessions", SESSION_ID + ".json");
  if (existsSync(source)) {
    mkdirSync(dirname(target), { recursive: true });
    copyFileSync(source, target);
  }
  const { state, unreadable } = readStateStrict(root, SESSION_ID);
  // The state travels as its text form, JSON.stringify(state, null, 2), the shape the port's
  // state.Encode prints, so both sides compare the same rebuilt state and the same rewritten bytes.
  const answer = { unreadable, state: mask(JSON.stringify(state, null, 2)) };
  try {
    writeState(root, state);
  } catch (error) {
    answer.writeError = error instanceof Error ? error.message : String(error);
    return answer;
  }
  try {
    answer.written = mask(readFileSync(statePath(root, SESSION_ID), "utf8"));
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
