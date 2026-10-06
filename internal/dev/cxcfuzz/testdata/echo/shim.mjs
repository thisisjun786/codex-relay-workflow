// The oracle worker for the echo target. The harness's worker pool starts it as `node shim.mjs`
// and it answers one NDJSON request per line on stdin with one reply per line on stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer              ->  {"id":1,"error":{"name":...,"message":...}}
//
// It is the echo target's oracle side: the answer is the input itself. Two environment variables
// make it disagree on purpose, which is how the harness's own tests prove a divergence is found
// and that a stalled request is timed out:
//   CXCFUZZ_MUTATE=1  appends "!" to the "text" string of an input carrying "mutate": true
//   CXCFUZZ_STALL=1   leaves the first request unanswered
//
// A real target's shim imports its dist module under ORACLE_ROOT the way the record-oracle.mjs
// recorders do; ORACLE_ROOT defaults to the extracted CXC v0.2.40 component tree. echo imports
// none, so the harness's own tests need no oracle tree.
import { createInterface } from "node:readline";

const oracleRoot = process.env.ORACLE_ROOT || "/var/tmp/cxc-v0.2.40/plugins/codexclaw/components";
const mutate = process.env.CXCFUZZ_MUTATE === "1";
const stall = process.env.CXCFUZZ_STALL === "1";

let stalled = false;

// echo is the answer: the input itself, with the deliberate mutation switched on.
function echo(input) {
  if (!mutate || input === null || typeof input !== "object" || Array.isArray(input)) return input;
  if (input.mutate !== true || typeof input.text !== "string") return input;
  return { ...input, text: input.text + "!" };
}

// run puts the homes the case declared under its own root, so a shim never reads a real one.
function run(request) {
  if (typeof request.root === "string" && request.root !== "") {
    process.env.HOME = request.root + "/home";
    process.env.CODEX_HOME = request.root + "/codex-home";
    process.env.CRW_HOME = request.root + "/crw-home";
    process.env.TMPDIR = request.root + "/tmp";
  }
  return echo(request.input);
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
  if (stall && !stalled) {
    stalled = true;
    return; // never answers: the pool times the request out and starts another worker
  }
  try {
    process.stdout.write(JSON.stringify({ id: request.id, output: run(request) }) + "\n");
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: request.id, error: { name: error.name, message: error.message } }) + "\n");
  }
});
lines.on("close", () => process.exit(0));
void oracleRoot;
