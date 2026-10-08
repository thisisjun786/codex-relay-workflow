// The oracle worker for the memorygate target. The harness's worker pool starts it as
// `node shim.mjs` and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer              ->  {"id":1,"error":{"name":...,"message":...}}
//
// The answer is the oracle's handleMemoryWriteGate(payload, env) at CXC v0.2.40
// (plugins/codexclaw/components/pabcd-state/dist/memory-write-gate.js), imported under ORACLE_ROOT
// the way the record-oracle.mjs recorders do. The input's "payload" is a PreToolUse envelope whose
// paths carry the literal ${ROOT}; the shim replaces it with this case's own root and puts the homes
// under that root, so a case never reads a real one. The scenario's fs tree is materialised by the
// harness before the call; the shim only reads it. The gate is the oracle's fail-open dispatch: it
// denies an unauthorized memory write and answers "" for everything else.
import { createInterface } from "node:readline";

const oracleRoot = process.env.ORACLE_ROOT || "/var/tmp/cxc-v0.2.40/plugins/codexclaw/components";
const { handleMemoryWriteGate } = await import(oracleRoot + "/pabcd-state/dist/memory-write-gate.js");

function substitute(value, root) {
  if (typeof value === "string") return value.split("${ROOT}").join(root);
  if (Array.isArray(value)) return value.map((item) => substitute(item, root));
  if (value !== null && typeof value === "object") {
    const out = {};
    for (const [key, item] of Object.entries(value)) out[substitute(key, root)] = substitute(item, root);
    return out;
  }
  return value;
}

function run(request) {
  const root = typeof request.root === "string" ? request.root : "";
  if (root !== "") {
    process.env.HOME = root + "/home";
    process.env.CODEX_HOME = root + "/codex-home";
    process.env.CRW_HOME = root + "/crw-home";
    process.env.CODEXCLAW_HOME = root + "/codexclaw-home";
    process.env.TMPDIR = root + "/tmp";
  }
  const payload = substitute(request.input && request.input.payload, root);
  if (payload === null || typeof payload !== "object") return "";
  return handleMemoryWriteGate(JSON.stringify(payload), process.env);
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
