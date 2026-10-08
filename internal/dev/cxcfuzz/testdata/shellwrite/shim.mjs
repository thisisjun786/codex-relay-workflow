// The oracle worker for the shellwrite target. The harness's worker pool starts it as
// `node shim.mjs` and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer              ->  {"id":1,"error":{"name":...,"message":...}}
//
// The answer is the oracle's shellWriteDestinations(command) at CXC v0.2.40
// (plugins/codexclaw/components/pabcd-state/dist/shell-write-destinations.js), imported under
// ORACLE_ROOT the way the record-oracle.mjs recorders do. The input's "command" is a shell command
// whose absolute destinations carry the literal ${ROOT}; the shim replaces it with this case's own
// root, so the harness can build the same relative tree under two roots and rewrite each side's root
// back to ${ROOT} before the two answers are compared. A generated command is never executed.
import { createInterface } from "node:readline";

const oracleRoot = process.env.ORACLE_ROOT || "/var/tmp/cxc-v0.2.40/plugins/codexclaw/components";
// The import runs once, here, before the listener exists, so a worker's first reply proves the oracle
// is loaded (the pool's readiness probe depends on it). A load that fails is remembered rather than
// thrown: the worker still answers the start-up handshake, which touches no oracle function, and answers
// every case with the remembered error (CRW-932; the spawn shim loads the same way).
let shellWriteDestinations;
let oracleLoadError = null;
try {
  ({ shellWriteDestinations } = await import(oracleRoot + "/pabcd-state/dist/shell-write-destinations.js"));
} catch (error) {
  oracleLoadError = error;
}

// run puts the homes the case declared under its own root, so a shim never reads a real one.
function run(request) {
  const root = typeof request.root === "string" ? request.root : "";
  if (root !== "") {
    process.env.HOME = root + "/home";
    process.env.CODEX_HOME = root + "/codex-home";
    process.env.CRW_HOME = root + "/crw-home";
    process.env.CODEXCLAW_HOME = root + "/codexclaw-home";
    process.env.TMPDIR = root + "/tmp";
  }
  const input = request.input;
  if (input === null || typeof input !== "object" || typeof input.command !== "string") {
    return [];
  }
  if (oracleLoadError) throw oracleLoadError;
  return shellWriteDestinations(input.command.split("${ROOT}").join(root)).map((dest) => String(dest));
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
