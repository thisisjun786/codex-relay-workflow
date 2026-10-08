// The oracle worker for the worktreedel target. The harness's worker pool starts it as
// `node shim.mjs` and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<any>,"root":"<case root>"}  ->  {"id":1,"output":<any>}
//   a request it cannot answer              ->  {"id":1,"error":{"name":...,"message":...}}
//
// The oracle is CXC v0.2.40's pabcd-state/dist/worktree-guard.js, imported under ORACLE_ROOT the way
// the record-oracle.mjs recorders import their dist modules. The target's Go side is
// internal/pabcd/hook's HandleWorktreeGuardPreTool, whose port speaks CRW names; the oracle speaks
// CXC names, so the answer's text goes through the same rename the corpus recorders apply
// (contract/schema/cxc/name-substitution.json, rules R2 and R32) before it is compared.
import { createInterface } from "node:readline";
import { join } from "node:path";
import { realpathSync } from "node:fs";

// The harness always sets ORACLE_ROOT from the target's Oracle.Root, so the worker needs no
// built-in path: the oracle tree is a caller-supplied input, not a value of this repository.
const oracleRoot = process.env.ORACLE_ROOT;
// The import runs once, here, before the listener exists, so a worker's first reply proves the oracle
// is loaded (the pool's readiness probe depends on it). A load that fails is remembered rather than
// thrown: the worker still answers the start-up handshake, which touches no oracle function, and answers
// every case with the remembered error (CRW-932; the spawn shim loads the same way).
let guard;
let oracleLoadError = null;
try {
  if (!oracleRoot) throw new Error("ORACLE_ROOT is not set");
  guard = await import("file://" + join(oracleRoot, "pabcd-state", "dist", "worktree-guard.js"));
} catch (error) {
  oracleLoadError = error;
}

// The rename the port's corpus applies to an expectation: every "codexclaw:cxc-X" becomes
// "crw:crw-X" first, then every "codexclaw" becomes "crw". Both are the rules the replayer uses.
function rename(text) {
  return text.replaceAll("codexclaw:cxc-", "crw:crw-").replaceAll("codexclaw", "crw");
}

// asGivenRoot rewrites a resolved spelling of the case root back to the spelling the harness gave,
// which is the one the harness replaces with its own placeholder on both answers. The guard
// canonicalizes the cwd before it names the slot, so on a root reached through a link (macOS's
// /private/var, for one) the reason would otherwise carry the resolved path and the two sides would
// be written differently. The Go side does the same in worktreeDelAsGivenRoot.
function asGivenRoot(text, root) {
  if (root === "") return text;
  let resolved;
  try {
    resolved = realpathSync.native(root);
  } catch {
    return text;
  }
  return resolved === root ? text : text.split(resolved).join(root);
}

// The environment the guard reads: the case's own homes, then the case's extra worktree roots, with
// a listed variable deleted (which the oracle's resolveCodexHome reads as unset).
function answer(request) {
  const input = request.input;
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    return { decision: "allow", reason: "" };
  }
  if (oracleLoadError) throw oracleLoadError;
  const root = typeof request.root === "string" ? request.root : "";
  process.env.HOME = join(root, "home");
  process.env.CRW_HOME = join(root, "crw-home");
  process.env.CODEXCLAW_HOME = join(root, "codexclaw-home");
  process.env.TMPDIR = join(root, "tmp");
  delete process.env.CODEXCLAW_WORKTREE_ROOTS;
  // CODEX_HOME is deliberately absent, so the oracle's resolveCodexHome falls back to
  // homedir()/.codex = <root>/home/.codex, the layout the issue describes. The case's own env then
  // overrides a name, or unsets it with a JSON null.
  delete process.env.CODEX_HOME;
  if (input.env !== null && typeof input.env === "object" && !Array.isArray(input.env)) {
    for (const [name, value] of Object.entries(input.env)) {
      if (value === null) delete process.env[name];
      else if (typeof value === "string") process.env[name] = join(root, value);
    }
  }
  if (Array.isArray(input.worktree_roots)) {
    const roots = input.worktree_roots
      .filter((entry) => typeof entry === "string")
      .map((entry) => join(root, entry));
    if (roots.length > 0) process.env.CODEXCLAW_WORKTREE_ROOTS = roots.join(":");
  }
  const cwd = typeof input.cwd === "string" && input.cwd !== "" ? join(root, input.cwd) : "";
  const payload = {
    hook_event_name: typeof input.event === "string" ? input.event : "",
    cwd,
    tool_name: typeof input.tool === "string" ? input.tool : "",
    tool_input: { command: typeof input.command === "string" ? input.command : "" },
  };
  const out = guard.handleWorktreeGuardPreTool(JSON.stringify(payload));
  if (typeof out !== "string" || out.trim() === "") return { decision: "allow", reason: "" };
  let parsed;
  try {
    parsed = JSON.parse(out);
  } catch (error) {
    throw new Error("the oracle answered something that is not JSON: " + error.message);
  }
  const specific = parsed && parsed.hookSpecificOutput;
  if (!specific || specific.permissionDecision !== "deny") return { decision: "allow", reason: "" };
  return { decision: "deny", reason: asGivenRoot(rename(String(specific.permissionDecisionReason ?? "")), root) };
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
