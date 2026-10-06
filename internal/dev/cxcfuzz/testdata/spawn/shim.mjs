// The oracle worker for the spawn target. The harness's worker pool starts it as `node shim.mjs`
// and it answers one NDJSON request per line on stdin with one reply per line on stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer              ->  {"id":1,"error":{"name":...,"message":...}}
//
// The oracle is CXC v0.2.40's subagent-config/dist/spawn-attach-hook.js, imported under ORACLE_ROOT.
// The target's Go side is internal/role/spawn, whose port speaks CRW names; the oracle speaks CXC
// names, so the request's text is translated to the oracle's spellings before the call and the
// answer's text back afterwards, with the rules of contract/schema/cxc/name-substitution.json the
// way the corpus recorders and internal/role/spawn/testdata/inline/record.mjs apply them.
import { createInterface } from "node:readline";
import { join } from "node:path";

// The harness always sets ORACLE_ROOT from the target's Oracle.Root, so the worker needs no
// built-in path: the oracle tree is a caller-supplied input, not a value of this repository.
const oracleRoot = process.env.ORACLE_ROOT;
if (!oracleRoot) throw new Error("ORACLE_ROOT is not set");
const hook = await import("file://" + join(oracleRoot, "subagent-config", "dist", "spawn-attach-hook.js"));

// The exported functions the issue names: each has an exported counterpart here. The Go side's
// StripControlMarkers and DenyEnvelope are not in this table: stripControlMarkers (409) and
// denyEnvelope (450) are internal to the oracle, and the issue's rule leaves a helper without an
// exported counterpart out of the target.
const functions = {
  InferRole: (agentType, message) => hook.inferRole(agentType, typeof message === "string" ? message : ""),
  IsV2SpawnInput: (toolInput) => hook.isV2SpawnInput(asObject(toolInput)),
  IsFullHistoryFork: (toolInput) => hook.isFullHistoryFork(asObject(toolInput)),
  IsSpawnToolName: (name) => hook.isSpawnToolName(name),
  IsCollaborationToolName: (name) => hook.isCollaborationToolName(name),
  MentionedFolders: (message) => [...hook.mentionedFolders(typeof message === "string" ? message : "")],
};

function asObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value : {};
}

// The port emits CRW names where the oracle emits CXC names, so a message means the same thing to
// both sides only after the rename table (contract/schema/cxc/name-substitution.json) is applied:
// a mention prefix becomes the oracle's spelling, a role marker becomes CXC-ROLE:, the recursion
// tokens keep their case-sensitive spelling, and a skill folder drops the crw- the table's R3 note
// adds (the note is that the folder X becomes crw-X, so the direction into the oracle is X). The
// mention prefix is translated case-insensitively, because the oracle's pattern carries the /i flag
// and the port spells its own classes [Cc][Rr][Ww], so both accept any case there.
function crwToCxc(text) {
  return text
    .replace(/\$crw:crw-/gi, "$codexclaw:cxc-")
    .replace(/\$crw-/gi, "$cxc-")
    .replaceAll("CRW-ROLE:", "CXC-ROLE:")
    .replaceAll("CRW-SUBSPAWN-ALLOWED", "CXC-SUBSPAWN-ALLOWED")
    .replaceAll("[CRW-SUBSPAWN-GRANT:", "[CXC-SUBSPAWN-GRANT:")
    .replace(/skill:\/\/([^\s]*?)\/crw-([^/\s)]+)\/SKILL\.md/gi, (whole, prefix, folder) => "skill://" + prefix + "/" + folder + "/SKILL.md");
}

// cxcToCrw is the table's forward direction for a skill folder: the folder X becomes crw-X, which
// is the name the port's MentionedFolders answers.
function cxcToCrw(folder) {
  return "crw-" + folder;
}

// sortFolders orders folder names the way the Go side does: by the bytes of their UTF-8 encoding.
// JavaScript's Array.sort compares UTF-16 code units, which puts an astral character before a BMP
// one where Go puts it after, so the two sides must share this comparator for a set comparison to
// mean anything.
function sortFolders(folders) {
  return [...folders].sort((a, b) => Buffer.compare(Buffer.from(a, "utf8"), Buffer.from(b, "utf8")));
}

function answer(request) {
  const input = request.input;
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    return refusal();
  }
  const fn = functions[input.fn];
  if (!fn) return refusal();
  const args = Array.isArray(input.args) ? input.args : [];
  const translated = args.map((argument) => (typeof argument === "string" ? crwToCxc(argument) : argument));
  const value = fn(...translated);
  if (input.fn === "MentionedFolders") return sortFolders(value.map(cxcToCrw));
  return value;
}

// The answer both sides give an input outside the grammar. It is answered rather than thrown,
// because the harness compares an answer's bytes and the two runtimes' error envelopes (Go's
// "GoError" against V8's error name) can never match.
function refusal() {
  return "the input is outside the target's grammar";
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
    process.stdout.write(JSON.stringify({ id: request.id, output: answer(request) }) + "\n");
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: request.id, error: { name: error.name, message: error.message } }) + "\n");
  }
});
lines.on("close", () => process.exit(0));
