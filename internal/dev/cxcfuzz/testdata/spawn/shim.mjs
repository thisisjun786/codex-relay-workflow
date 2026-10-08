// The oracle worker for the spawn target. The harness's worker pool starts it as `node shim.mjs`
// and it answers one NDJSON request per line on stdin with one reply per line on stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer              ->  {"id":1,"error":{"name":...,"message":...}}
//
// The oracle is CXC v0.2.40's subagent-config/dist/spawn-attach-hook.js, imported under ORACLE_ROOT.
// The target's Go side is internal/role/spawn, whose port speaks CRW names; the oracle speaks CXC
// names, so the request's text is translated into the spellings the oracle's recognizers read, with
// the rules of contract/schema/cxc/name-substitution.json the way the corpus recorders and
// internal/role/spawn/testdata/inline/record.mjs apply them. The answer needs no translation at all.
import { createInterface } from "node:readline";
import { appendFileSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The oracle is loaded once here, before the stdin listener exists, so its module initialization is paid
// by the worker's start-up budget rather than by a case's timeout: the pool charges everything up to the
// handshake reply to the startup deadline (worker.go ready/acquire) and only what follows to the per-case
// one, so a load moved into a case would be charged to that case and a slow import would time out a
// worker that answered its handshake. There is exactly one attempt: a later attempt, made while a case
// waits, would put that same cost on the case, so a worker whose load failed keeps listening and answers
// every later request with the remembered error instead of trying again.
//
// The load must not run under the caller's environment. The pool hands the worker the caller's
// environment (Campaign passes os.Environ() through NewPool), so before the import the worker creates a
// temporary root of its own under the harness TMPDIR and points HOME, CODEX_HOME, CRW_HOME,
// CODEXCLAW_HOME and TMPDIR at it. The oracle's module initialization therefore runs under a root this
// worker owns and removes again when the attempt ends. Each case then gets its own homes: run puts the
// five variables under request.root before the classifier is called, after the load has finished, so a
// case's own work reads the case's tree and not the load root.
//
// A load that fails is remembered, not fatal: the worker still starts, still answers the pool's
// start-up handshake (a null input with an empty root) with the refusal, and answers a later request
// with an error envelope, so a missing or hidden oracle tree reads as an answer rather than as a dead
// worker.
//
// The harness temporary directory the pool handed this worker, captured here before any request can
// replace TMPDIR, so the load root is always created under the harness's own scratch directory. A TMPDIR
// the worker cannot make its root under is an initialization failure it remembers and answers; it is
// never a reason to create the root somewhere else, which would put the oracle's initialization outside
// the boundary the harness selected. When the caller named no TMPDIR at all, the process's own temporary
// directory is that scratch.
const loadBase = typeof process.env.TMPDIR === "string" && process.env.TMPDIR !== "" ? process.env.TMPDIR : tmpdir();
const loadHomes = ["home", "codex-home", "crw-home", "codexclaw-home", "tmp"];
let oracleTable = null;
let oracleLoadError = null;

// setHomes points the five variables a case's homes live in at one root.
function setHomes(root) {
  process.env.HOME = join(root, "home");
  process.env.CODEX_HOME = join(root, "codex-home");
  process.env.CRW_HOME = join(root, "crw-home");
  process.env.CODEXCLAW_HOME = join(root, "codexclaw-home");
  process.env.TMPDIR = join(root, "tmp");
}

// recordLoadRoot appends the root one import attempt used to the file CXCFUZZ_LOAD_ROOTS names, when
// the harness sets it, so a test can read back which roots the worker created. With the variable unset
// the worker does no such write.
function recordLoadRoot(root) {
  const path = process.env.CXCFUZZ_LOAD_ROOTS;
  if (typeof path !== "string" || path === "") return;
  try {
    appendFileSync(path, root + "\n");
  } catch {
    // A record that cannot be written must not stop the worker.
  }
}

// makeLoadRoot creates a fresh, exclusively owned root for the import and points the five variables at
// it. mkdtempSync creates a new 0700 directory and fails rather than following a pathname that already
// exists, so a name another process has since taken over is never inherited and no import ever creates a
// directory through a path this worker does not own. A root that cannot be completed is removed again
// before the error is raised, on a best-effort basis (see releaseLoadRoot). The caller removes the root when
// the import ends.
function makeLoadRoot() {
  const root = mkdtempSync(join(loadBase, "crw-spawn-load-"));
  try {
    for (const name of loadHomes) {
      mkdirSync(join(root, name), { recursive: true });
    }
  } catch (error) {
    releaseLoadRoot(root);
    throw error;
  }
  recordLoadRoot(root);
  setHomes(root);
  return root;
}

// releaseLoadRoot removes one attempt's root. The root is this worker's own scratch: a failure to remove
// it must not stop the worker, so the removal is best-effort and its error is dropped. A root is left
// behind when this process is killed outright (the pool's startup deadline sends SIGKILL, which no finally
// can run under) or when the removal itself fails; either way the leftover lives under the harness TMPDIR
// the caller chose.
function releaseLoadRoot(root) {
  try {
    rmSync(root, { recursive: true, force: true });
  } catch {
    // best-effort cleanup
  }
}

async function loadOracle() {
  const root = makeLoadRoot();
  try {
    const base = process.env.ORACLE_ROOT;
    if (!base) throw new Error("ORACLE_ROOT is not set");
    return functions(await import("file://" + join(base, "subagent-config", "dist", "spawn-attach-hook.js")));
  } finally {
    releaseLoadRoot(root);
  }
}

try {
  oracleTable = await loadOracle();
} catch (error) {
  oracleLoadError = error;
}

// oracle is the table the one load attempt produced, or the error it left. There is no second attempt
// (see the note on the load above): a worker whose load failed answers every later request with the
// remembered error and keeps listening.
function oracle() {
  if (oracleTable) return oracleTable;
  throw oracleLoadError ?? new Error("the oracle was not loaded");
}

// The exported functions the issue names: each has an exported counterpart here. The Go side's
// StripControlMarkers and DenyEnvelope are not in this table: stripControlMarkers (409) and
// denyEnvelope (450) are internal to the oracle, and the issue's rule leaves a helper without an
// exported counterpart out of the target.
function functions(hook) {
  return {
    InferRole: (agentType, message) => hook.inferRole(agentType, typeof message === "string" ? message : ""),
    IsV2SpawnInput: (toolInput) => hook.isV2SpawnInput(asObject(toolInput)),
    IsFullHistoryFork: (toolInput) => hook.isFullHistoryFork(asObject(toolInput)),
    IsSpawnToolName: (name) => hook.isSpawnToolName(name),
    IsCollaborationToolName: (name) => hook.isCollaborationToolName(name),
    MentionedFolders: (message) => [...hook.mentionedFolders(typeof message === "string" ? crwToCxc(message) : "")],
  };
}

function asObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value : {};
}

// crwToCxc rewrites the request text into the spellings the oracle's recognizers read.
//
// The mention prefix carries the whole rename. The port's bare-mention pattern captures the part
// after "crw-" and answers "crw-" plus the lowercased capture; the oracle's captures after "cxc-"
// and answers the lowercased capture alone. The table's note is that the folder X becomes crw-X, so
// the direction into the oracle is X for crw-X; prefixing the capture with "crw-" after the "cxc-"
// the pattern needs makes the oracle answer exactly the name the port answers, with no guesswork on
// the way out. A skill:// link's folder is captured verbatim by both patterns and answered verbatim
// by both, so it is left alone. The role marker (R9) and the recursion token and grant marker (R7,
// R6) are translated because the recognizers read their spelling. The prefix is translated
// case-insensitively, because the oracle's pattern carries the /i flag and the port spells its own
// classes [Cc][Rr][Ww], so both accept any case there.
function crwToCxc(text) {
  return text
    .replace(/\$crw:crw-/gi, "$codexclaw:cxc-crw-")
    .replace(/\$crw-/gi, "$cxc-crw-")
    .replaceAll("CRW-ROLE:", "CXC-ROLE:")
    .replaceAll("CRW-SUBSPAWN-ALLOWED", "CXC-SUBSPAWN-ALLOWED")
    .replaceAll("[CRW-SUBSPAWN-GRANT:", "[CXC-SUBSPAWN-GRANT:");
}

// sortFolders orders folder names the way the Go side does: by the bytes of their UTF-8 encoding,
// with a lone surrogate kept as the three WTF-8 bytes a Go string holds it in rather than replaced
// by U+FFFD. JavaScript's Array.sort compares UTF-16 code units, which puts an astral character
// before a BMP one where Go puts it after, so the two sides share this comparator instead.
function sortFolders(folders) {
  return [...folders].sort((a, b) => Buffer.compare(wtf8(a), wtf8(b)));
}

// wtf8 is a string as its UTF-8 bytes, with every lone surrogate kept as the three-byte sequence a
// Go string holds it in. Node's Buffer.from(text, "utf8") writes U+FFFD for a lone surrogate, which
// would make two distinct surrogate folders compare equal and leave them in encounter order.
function wtf8(text) {
  const bytes = [];
  for (let i = 0; i < text.length; i += 1) {
    const unit = text.charCodeAt(i);
    if (unit >= 0xd800 && unit <= 0xdbff && i + 1 < text.length) {
      const low = text.charCodeAt(i + 1);
      if (low >= 0xdc00 && low <= 0xdfff) {
        const point = 0x10000 + ((unit - 0xd800) << 10) + (low - 0xdc00);
        bytes.push(0xf0 | (point >> 18), 0x80 | ((point >> 12) & 0x3f), 0x80 | ((point >> 6) & 0x3f), 0x80 | (point & 0x3f));
        i += 1;
        continue;
      }
    }
    if (unit >= 0xd800 && unit <= 0xdfff) {
      bytes.push(0xe0 | (unit >> 12), 0x80 | ((unit >> 6) & 0x3f), 0x80 | (unit & 0x3f));
      continue;
    }
    if (unit < 0x80) bytes.push(unit);
    else if (unit < 0x800) bytes.push(0xc0 | (unit >> 6), 0x80 | (unit & 0x3f));
    else bytes.push(0xe0 | (unit >> 12), 0x80 | ((unit >> 6) & 0x3f), 0x80 | (unit & 0x3f));
  }
  return Buffer.from(bytes);
}

function answer(table, input) {
  const fn = table[input.fn];
  if (!fn) return refusal();
  const args = Array.isArray(input.args) ? input.args : [];
  const translated = args.map((argument) => (typeof argument === "string" ? crwToCxc(argument) : argument));
  const value = fn(...translated);
  if (input.fn === "MentionedFolders") return sortFolders(value);
  return value;
}

// run isolates one case and answers it. The harness hands the worker pool the caller's environment
// (Campaign passes os.Environ() through NewPool), so without this step a case would answer under the
// real HOME, CODEX_HOME, CRW_HOME, CODEXCLAW_HOME and TMPDIR. The echo, memorygate and doctor shims
// each put those five under request.root per request; this does the same, so a case's own work reads
// the case's tree. An input that is not an object is answered with the refusal first and touches
// nothing else: the pool's start-up handshake is {"id":N,"input":null,"root":""}, a readiness probe
// that must never read or write a home. The oracle is consulted before the case's homes are set,
// because loading it runs the oracle's module initialization under a root of the worker's own (see
// loadOracle); setting the case's homes first would be overwritten by that root and then left pointing
// at a directory the worker has already removed.
async function run(request) {
  const input = request.input;
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    return refusal();
  }
  // The oracle is consulted before the case's homes are set, because loading it runs the oracle's module
  // initialization under the worker's own root (see loadOracle). Setting the case's homes first would be
  // overwritten by that root and then left pointing at a directory this worker has already removed, so
  // the classifier would read a tree that is not the case's.
  const table = oracle();
  const root = typeof request.root === "string" ? request.root : "";
  if (root !== "") {
    setHomes(root);
  }
  return answer(table, input);
}

// The answer both sides give an input outside the grammar. It is answered rather than thrown,
// because the harness compares an answer's bytes and the two runtimes' error envelopes (Go's
// "GoError" against V8's error name) can never match.
function refusal() {
  return "the input is outside the target's grammar";
}

const lines = createInterface({ input: process.stdin, terminal: false });
lines.on("line", async (line) => {
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
    process.stdout.write(JSON.stringify({ id: request.id, output: await run(request) }) + "\n");
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: request.id, error: { name: error.name, message: error.message } }) + "\n");
  }
});
lines.on("close", () => process.exit(0));
