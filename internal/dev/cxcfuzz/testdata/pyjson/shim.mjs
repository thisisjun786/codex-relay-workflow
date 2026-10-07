// The oracle worker for the pyjson target. The harness's worker pool starts it as
// `node shim.mjs` and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer                   ->  {"id":1,"error":{"name":...,"message":...}}
//   {"id":N,"input":null,"root":""}              ->  {"id":N,"output":null}   (start-up handshake)
//
// The pool's start-up handshake carries a null input and an empty root, and its reply is discarded;
// the shim answers it inertly and spawns no python3 for it (CRW-854).
//
// The oracle is the Python standard library's json module, reached through
// `python3 -m json.tool`, one python3 process per request, the case's JSON text on its stdin. Its
// exit status, stdout and stderr are the answer. No .py file and no Python source text is committed
// anywhere: this file only builds an argv and pipes the input in.
//
// The input is:
//   {"text":"<json document>","indent":N,"compact":bool,"sortKeys":bool,"ensureAscii":bool}
// indent 4 is json.tool's default and adds no flag; indent 0 is --no-indent; any other non-negative
// indent is --indent N. compact wins over indent, as json.tool refuses the two together. json.tool
// always writes one trailing newline after the value; it is stripped here, so both sides compare the
// value alone.
import { createInterface } from "node:readline";
import { spawnSync } from "node:child_process";

// run puts the homes the case declared under its own root, so a shim never reads a real one.
function run(request) {
  // The pool's start-up handshake is one request with a null input and an empty root (CRW-854). It
  // is a readiness probe whose reply is discarded, so answer it inertly without running python3:
  // the handshake must not pay for an oracle process per worker start.
  const handshake = request.input;
  if (handshake === null || typeof handshake !== "object" || Array.isArray(handshake)) {
    return null;
  }
  const root = typeof request.root === "string" ? request.root : "";
  if (root !== "") {
    process.env.HOME = root + "/home";
    process.env.CODEX_HOME = root + "/codex-home";
    process.env.CRW_HOME = root + "/crw-home";
    process.env.CODEXCLAW_HOME = root + "/codexclaw-home";
    process.env.TMPDIR = root + "/tmp";
  }
  const input = request.input;
  // A missing or non-string text is the empty document, exactly as the Go side reads it, so a
  // shrunk input that dropped the field still compares a document rather than a protocol error.
  const text = input !== null && typeof input === "object" && typeof input.text === "string" ? input.text : "";
  // No --json-lines: the oracle is json.loads/json.dumps over the whole document, and --json-lines
  // would make it a line-by-line parser (accepting zero lines and refusing a pretty-printed document),
  // which is not the reader pyjson.Loads replaces.
  const argv = ["-m", "json.tool"];
  if (input.compact === true) {
    argv.push("--compact");
  } else if (input.indent === 0) {
    argv.push("--no-indent");
  } else if (typeof input.indent === "number" && Number.isInteger(input.indent) && input.indent !== 4) {
    argv.push("--indent", String(input.indent));
  }
  if (input.sortKeys === true) argv.push("--sort-keys");
  if (input.ensureAscii === false) argv.push("--no-ensure-ascii");
  // maxBuffer is raised well past Node's 1 MiB default: the generator draws documents nested near
  // Python's recursion limit (CRW-708 generation 5, d2), and json.tool's re-dumped value and its
  // RecursionError traceback both grow with the nesting. With the default the oracle answers an
  // ENOBUFS error instead of its real answer, which the campaign then records as a difference
  // between the two sides rather than as the harness's own buffer limit.
  const result = spawnSync("python3", argv, { input: text, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  if (result.error) throw result.error;
  let stdout = result.stdout ?? "";
  if (stdout.endsWith("\n")) stdout = stdout.slice(0, -1);
  // json.tool writes the value and its error text each with one trailing newline; both are stripped,
  // so both sides compare the value or the message alone.
  let stderr = result.stderr ?? "";
  if (stderr.endsWith("\n")) stderr = stderr.slice(0, -1);
  return { exit: result.status ?? 1, stdout, stderr };
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
