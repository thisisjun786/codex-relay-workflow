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
// `python3 -m json.tool --json-lines`, the case's JSON text on its stdin. Its exit status, stdout and
// stderr are the answer. No .py file and no Python source text is committed anywhere: this file only
// builds an argv and pipes the input in.
//
// The input is:
//   {"text":"<json document>","indent":N,"compact":bool,"sortKeys":bool,"ensureAscii":bool}
// indent 4 is json.tool's default and adds no flag; indent 0 is --no-indent; any other non-negative
// indent is --indent N. compact wins over indent, as json.tool refuses the two together. json.tool
// always writes one trailing newline after the value; it is stripped here, so both sides compare the
// value alone.
//
// The answer to a request is the answer of a fresh `python3 -m json.tool --json-lines` process given
// that request's text (CRW-1066). Three paths reach it, from the cheapest, and each gives the same exit
// status, stdout and stderr:
//   1. The answer for the same argv and text, remembered. json.tool is a pure function of both, so a
//      repeated request (the generator draws from a small set of boundary documents, and the shrinker
//      asks the same candidates again) is not asked of Python twice.
//   2. A reused process. One json.tool process per argv stays open and reads one request after another:
//      the request's text, its last line ended, then a sentinel document no request can contain. Python
//      prints each document's re-dumped value as it reads it, so the sentinel's own echo marks the end
//      of the answer; what comes before it is stdout. This costs a process start only once per argv
//      instead of once per request. Only a text whose every line the host JSON parser accepts, and that
//      cannot nest near Python's recursion limit, is offered to a reused process, and only a clean
//      answer is taken from one: json.tool ends the process on any refusal, and a refusal's message
//      names a line and column that the line ending this path adds would move, so the refused request
//      is asked again below and a refusal is always the fresh process's own text.
//   3. A fresh process per request, the text on its stdin and its end of input as the end of the last
//      line. This is the reference answer, and the only path for a refusal, a document the host parser
//      rejects (NaN, Infinity), a deep one, and an answer too large to remember.
//
// A reused process that is killed or stops is a failure of the harness's worker, not an answer: the shim
// exits without a reply when it finds a reused process ended by a signal, so the pool counts a dead
// worker and starts another; a reused process that stops answering is a request that outlives the
// pool's deadline, and the pool kills the worker and its process group.
import { createInterface } from "node:readline";
import { spawn, spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { gunzipSync, gzipSync } from "node:zlib";

// The environment a reused process starts under: the one this worker started under, which the harness
// built under its own start-up root. A request's root is removed when the case ends, so a process that
// outlives the case never holds it.
const baseEnv = { ...process.env };

// maxBuffer is raised well past Node's 1 MiB default: the generator draws documents nested near
// Python's recursion limit (CRW-708 generation 5, d2), and json.tool's re-dumped value and its
// RecursionError traceback both grow with the nesting. With the default the oracle answers an
// ENOBUFS error instead of its real answer, which the campaign then records as a difference
// between the two sides rather than as the harness's own buffer limit.
const MAX_BUFFER = 64 * 1024 * 1024;

// At most this many json.tool processes stay open, one per argv; the one used least recently ends when a
// new argv needs a place.
const REUSED_LIMIT = 8;

// A text with this many container openers or more may nest near Python's recursion limit (1000), where
// the refusal or the answer depends on the stack the dump runs on; it is asked of a fresh process.
const REUSED_OPENER_LIMIT = 256;

// A text longer than this is asked of a fresh process.
const REUSED_TEXT_LIMIT = 64 * 1024;

// The remembered answers: a text at most this long, an answer at most MEMO_ANSWER_LIMIT bytes whole, the
// store at most MEMO_BYTES bytes. An answer over MEMO_PLAIN bytes is kept compressed.
const MEMO_TEXT_LIMIT = 8 * 1024;
const MEMO_PLAIN = 16 * 1024;
const MEMO_ANSWER_LIMIT = 8 * 1024 * 1024;
const MEMO_BYTES = 16 * 1024 * 1024;

// The shim ends the request it cannot answer without a reply, so the pool sees a worker that died.
class WorkerFailure extends Error {}

function argvFor(input) {
  // --json-lines is the oracle's reader as the criterion names it (CRW-708 generation 5, c10 d1): each
  // line is one document, so an empty document is zero documents and a pretty-printed one is refused
  // at its second line. Where the port's whole-document reader answers differently, the case is a
  // divergence the cases pin, not a shim choice.
  const argv = ["-m", "json.tool", "--json-lines"];
  if (input.compact === true) {
    argv.push("--compact");
  } else if (input.indent === 0) {
    argv.push("--no-indent");
  } else if (typeof input.indent === "number" && Number.isInteger(input.indent) && input.indent !== 4) {
    argv.push("--indent", String(input.indent));
  }
  if (input.sortKeys === true) argv.push("--sort-keys");
  if (input.ensureAscii === false) argv.push("--no-ensure-ascii");
  return argv;
}

// strip drops the one trailing newline json.tool writes after the value and after its error text, so
// both sides compare the value or the message alone.
function strip(text) {
  return text.endsWith("\n") ? text.slice(0, -1) : text;
}

// fresh asks a new python3 process, the text on its stdin.
function fresh(argv, text) {
  const result = spawnSync("python3", argv, { input: text, encoding: "utf8", maxBuffer: MAX_BUFFER });
  if (result.error) throw result.error;
  return { exit: result.status ?? 1, stdout: strip(result.stdout ?? ""), stderr: strip(result.stderr ?? "") };
}

// offered says whether a text may be asked of a reused process. This only routes: a text offered that
// Python then refuses is asked again fresh, so a wrong guess costs time and never changes an answer.
function offered(text) {
  if (text.length > REUSED_TEXT_LIMIT) return false;
  let openers = 0;
  for (let i = 0; i < text.length; i++) {
    const ch = text.charCodeAt(i);
    if (ch === 0x5b || ch === 0x7b) openers++;
  }
  if (openers >= REUSED_OPENER_LIMIT) return false;
  if (text === "") return true;
  // json.tool reads lines and a blank line is a refusal; a line the host parser rejects is almost always
  // one Python rejects. Either ends the process, so such a text goes to a fresh one directly.
  const pieces = text.split(/\r\n|\r|\n/);
  if (pieces[pieces.length - 1] === "") pieces.pop();
  for (const piece of pieces) {
    try {
      JSON.parse(piece);
    } catch {
      return false;
    }
  }
  return true;
}

// A reused json.tool process: it reads the request text and a sentinel document, and prints each
// document's value as it goes (python3 -u), so the sentinel's echo ends one answer.
class Reused {
  constructor(argv) {
    this.argv = argv;
    this.token = randomBytes(12).toString("hex");
    this.count = 0;
    this.out = "";
    this.err = "";
    this.ended = null;
    this.retired = false;
    this.pending = null;
    this.child = spawn("python3", ["-u", ...argv], { env: baseEnv, stdio: ["pipe", "pipe", "pipe"] });
    this.child.stdout.setEncoding("utf8");
    this.child.stderr.setEncoding("utf8");
    this.child.stdout.on("data", (chunk) => {
      this.out += chunk;
      this.settle();
    });
    this.child.stderr.on("data", (chunk) => {
      this.err += chunk;
    });
    this.child.stdin.on("error", () => {});
    this.child.on("error", (error) => {
      this.ended = { error };
      this.settle();
    });
    this.child.on("close", (code, signal) => {
      this.ended = { code, signal };
      this.settle();
    });
  }

  // ask writes one request and resolves with the answer, or null when the process ended by itself with an
  // exit status (a refusal, which the caller asks again fresh), or rejects when it was ended by a signal.
  ask(text) {
    return new Promise((resolve, reject) => {
      this.pending = { resolve, reject };
      if (this.ended !== null) {
        this.settle();
        return;
      }
      this.out = "";
      this.err = "";
      this.count++;
      this.sentinel = JSON.stringify(`crw-${this.token}-${this.count}`) + "\n";
      const ended = text === "" || text.endsWith("\n") ? text : text + "\n";
      this.child.stdin.write(ended + this.sentinel);
    });
  }

  settle() {
    const pending = this.pending;
    if (pending === null) return;
    if (this.ended !== null && this.ended.error) {
      this.pending = null;
      pending.reject(this.ended.error);
      return;
    }
    const sentinel = this.sentinel;
    if (this.ended === null && sentinel !== undefined && this.out.endsWith(sentinel) &&
        (this.out.length === sentinel.length || this.out[this.out.length - sentinel.length - 1] === "\n")) {
      this.pending = null;
      const stdout = this.out.slice(0, this.out.length - sentinel.length);
      const stderr = this.err;
      this.out = "";
      this.err = "";
      pending.resolve(stderr === "" ? { exit: 0, stdout: strip(stdout), stderr: "" } : null);
      return;
    }
    if (this.ended === null && this.out.length > MAX_BUFFER) {
      // An answer past the buffer the fresh path allows is asked fresh, which reports the limit as it always did.
      this.pending = null;
      this.child.kill("SIGKILL");
      pending.resolve(null);
      return;
    }
    if (this.ended !== null) {
      this.pending = null;
      if (this.ended.signal) {
        pending.reject(new WorkerFailure(`a reused json.tool process ended by ${this.ended.signal}`));
      } else {
        pending.resolve(null);
      }
    }
  }

  // retire lets a process that is no longer used end: it reads end of input and exits.
  retire() {
    this.retired = true;
    this.child.stdin.end();
  }
}

const reused = new Map();

// reusedFor is the open process for argv, a new one when there is none, and null-safe about the ones that ended:
// a process that ended while idle was ended by something other than a refusal, which is a worker failure.
function reusedFor(argv) {
  const key = argv.join("\u0000");
  let proc = reused.get(key);
  if (proc !== undefined) {
    reused.delete(key);
    if (proc.ended !== null) {
      if (proc.ended.error) throw proc.ended.error;
      throw new WorkerFailure(`a reused json.tool process ended between requests (${proc.ended.signal ?? "exit " + proc.ended.code})`);
    }
  } else {
    proc = new Reused(argv);
    while (reused.size >= REUSED_LIMIT) {
      const oldest = reused.keys().next().value;
      reused.get(oldest).retire();
      reused.delete(oldest);
    }
  }
  reused.set(key, proc);
  return proc;
}

function forget(argv, proc) {
  const key = argv.join("\u0000");
  if (reused.get(key) === proc) reused.delete(key);
}

// The remembered answers, oldest first.
const memo = new Map();
let memoBytes = 0;

function remember(key, answer) {
  const size = answer.stdout.length + answer.stderr.length;
  if (key.length > MEMO_TEXT_LIMIT + 512 || size > MEMO_ANSWER_LIMIT) return;
  if (memo.has(key)) return;
  const stored = size > MEMO_PLAIN ? { gz: gzipSync(JSON.stringify(answer), { level: 1 }) } : { answer };
  const bytes = stored.gz ? stored.gz.length : size;
  stored.bytes = bytes + key.length;
  memo.set(key, stored);
  memoBytes += stored.bytes;
  while (memoBytes > MEMO_BYTES && memo.size > 1) {
    const oldest = memo.keys().next().value;
    memoBytes -= memo.get(oldest).bytes;
    memo.delete(oldest);
  }
}

function recall(key) {
  const stored = memo.get(key);
  if (stored === undefined) return null;
  memo.delete(key);
  memo.set(key, stored);
  return stored.gz ? JSON.parse(gunzipSync(stored.gz).toString("utf8")) : { ...stored.answer };
}

// answer is the fresh process's answer to argv and text, by the cheapest path that gives it.
async function answer(argv, text) {
  const key = argv.join("\u0000") + "\u0001" + text;
  const known = recall(key);
  if (known !== null) return known;
  let result = null;
  if (offered(text)) {
    const proc = reusedFor(argv);
    result = await proc.ask(text);
    if (result === null) forget(argv, proc);
  }
  if (result === null) result = fresh(argv, text);
  remember(key, result);
  return result;
}

// run puts the homes the case declared under its own root, so a shim never reads a real one.
async function run(request) {
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
  const text = typeof input.text === "string" ? input.text : "";
  return answer(argvFor(input), text);
}

function reply(message) {
  process.stdout.write(JSON.stringify(message) + "\n");
}

// Requests run one at a time, in the order they arrive.
let queue = Promise.resolve();

async function handle(line) {
  const text = line.trim();
  if (text === "") return;
  let request;
  try {
    request = JSON.parse(text);
  } catch (error) {
    reply({ id: null, error: { name: error.name, message: error.message } });
    return;
  }
  try {
    reply({ id: request.id, output: await run(request) });
  } catch (error) {
    if (error instanceof WorkerFailure) {
      // No reply: the pool reads the end of this worker as a dead worker and starts another.
      process.stderr.write(error.message + "\n");
      process.exit(1);
    }
    reply({ id: request.id, error: { name: error.name, message: error.message } });
  }
}

const lines = createInterface({ input: process.stdin, terminal: false });
lines.on("line", (line) => {
  queue = queue.then(() => handle(line));
});
lines.on("close", () => {
  queue.then(() => {
    for (const proc of reused.values()) proc.retire();
    process.exit(0);
  });
});
