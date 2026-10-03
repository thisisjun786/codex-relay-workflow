// Records what the CXC v0.2.40 oracle answers for the units of pabcd-state/src/subagent-evidence.ts lines 326-470, for the Go
// tests to replay from oracle-unrecordable.json (no Node at test time): writeUnrecordableMarker, unrecordableVerdictStatus (and
// markerDirWritable through it), hasSpentBudget, resolveTombstone, escalationDirective, and verifierDirective through the
// SubagentStop gate that is its only caller. Every case runs in a fresh work directory under the work root; "{S}" in a case is
// the state directory name (.codexclaw in the oracle, .crw in the port), "<CWD>" and "<OUT>" the case's workspace and an outside
// directory. Date is frozen so every time is 2026-01-01T00:00:00.000Z (1767225600000 ms). Recorded with Node v24 as
//   node record-oracle-unrecordable.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> cases-unrecordable.json > oracle-unrecordable.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdirSync, writeFileSync, readFileSync, readdirSync, existsSync, symlinkSync, chmodSync, lstatSync, readlinkSync } from "node:fs";
import { spawn } from "node:child_process";
import { join, dirname, isAbsolute } from "node:path";

const FROZEN = Date.parse("2026-01-01T00:00:00.000Z");
const RealDate = Date;
globalThis.Date = class extends RealDate {
  constructor(...a) { super(...(a.length ? a : [FROZEN])); }
  static now() { return FROZEN; }
};
const [oracleDist, workRoot, casesFile] = process.argv.slice(2);
const ev = await import(oracleDist + "/subagent-evidence.js");
const st = await import(oracleDist + "/state.js");
const cases = JSON.parse(readFileSync(casesFile, "utf8"));
const S = ".codexclaw";
let n = 0;
const sleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const fresh = () => {
  const root = join(workRoot, "c" + n++), ws = join(root, "ws"), out = join(root, "out");
  mkdirSync(ws, { recursive: true });
  mkdirSync(out);
  return { ws, out };
};
const sub = (s, c) => s.replaceAll("{S}", S).replaceAll("<CWD>", c.ws).replaceAll("<OUT>", c.out);
const at = (c, p) => { const s = sub(p, c); return isAbsolute(s) ? s : join(c.ws, s); };
const deep = (o) => (o.pre ?? "") + "[".repeat(o.deep) + "]".repeat(o.deep) + (o.post ?? "");
const body = (o) => (o.b64 !== undefined ? Buffer.from(o.b64, "base64") : o.deep !== undefined ? Buffer.from(deep(o)) : Buffer.from(o.text ?? ""));
const parent = (p) => mkdirSync(dirname(p), { recursive: true });
const setup = (c, ops) => {
  for (const o of ops ?? []) {
    const p = at(c, o.file ?? o.dir ?? o.symlink ?? o.chmod);
    if (o.dir) mkdirSync(p, { recursive: true });
    else if (o.file !== undefined) { parent(p); writeFileSync(p, body(o)); }
    else if (o.symlink) { parent(p); symlinkSync(sub(o.to, c), p); }
    else if (o.chmod) chmodSync(p, parseInt(o.mode, 8));
  }
};
const restore = (c, ops) => { for (const o of ops ?? []) if (o.chmod) chmodSync(at(c, o.chmod), 0o700); };
// walk lists a tree without following links: path, type, and the text of a marker file.
const walk = (root, base = root, acc = []) => {
  let names;
  try { names = readdirSync(root).sort(); } catch { return acc; }
  for (const name of names) {
    const p = join(root, name), rel = p.slice(base.length + 1), info = lstatSync(p);
    if (info.isSymbolicLink()) acc.push({ path: rel, type: "link", to: readlinkSync(p) });
    else if (info.isDirectory()) { acc.push({ path: rel, type: "dir" }); walk(p, base, acc); }
    else acc.push({ path: rel, type: "file", ...(rel.includes("evidence-unrecordable/") || rel.includes("evidence-attempts/") ? { text: readFileSync(p, "utf8") } : {}) });
  }
  return acc;
};
const tree = (c) => walk(join(c.ws, S)).map((e) => ({ ...e, path: e.path.replaceAll(c.ws, "<CWD>"), ...(e.to ? { to: e.to.replaceAll(c.out, "<OUT>").replaceAll(c.ws, "<CWD>") } : {}) }));
const outTree = (c) => walk(c.out);
const gitignoreText = (t) => t.map((e) => (e.path === ".gitignore" ? { ...e, text: undefined } : e));

const out = { constant: ev.EVIDENCE_UNRECORDABLE_SUBDIR, marker: {}, status: {}, budget: {}, resolve: {}, directives: {} };

for (const k of cases.marker) {
  const c = fresh(), threw = [];
  setup(c, k.setup);
  for (const ids of k.calls) {
    try { ev.writeUnrecordableMarker(c.ws, ids.session, ids.agent); threw.push(false); } catch (e) { threw.push(e.code ?? String(e)); }
  }
  restore(c, k.setup);
  out.marker[k.id] = { threw, tree: gitignoreText(tree(c)), out: outTree(c) };
}

for (const k of cases.status) {
  const c = fresh();
  setup(c, k.setup);
  const got = ev.unrecordableVerdictStatus(c.ws, k.session ?? "s1");
  restore(c, k.setup);
  out.status[k.id] = { present: got.present, unreadable: got.unreadable, tree: gitignoreText(tree(c)), out: outTree(c) };
}

for (const k of cases.budget) {
  const c = fresh();
  setup(c, k.setup);
  const spent = ev.hasSpentBudget(c.ws, k.session ?? "s1");
  restore(c, k.setup);
  out.budget[k.id] = { spent, tree: gitignoreText(tree(c)) };
}

const payload = (c, p) => ({ hook_event_name: "SubagentStop", session_id: "s1", cwd: c.ws, ...p });
for (const k of cases.resolve) {
  const c = fresh(), sess = join(c.ws, S, "sessions"), lock = join(sess, "s1.json.lock"), returns = [];
  let seed = null;
  if (k.seedOverrides) { st.writeState(c.ws, { ...st.defaultState("s1"), ...k.seedOverrides }); seed = readFileSync(join(sess, "s1.json"), "utf8"); }
  if (k.stateRaw !== undefined) { mkdirSync(sess, { recursive: true }); writeFileSync(join(sess, "s1.json"), k.stateRaw); }
  if (k.stateDirIsFile) writeFileSync(join(c.ws, S), "x");
  if (k.lock) { mkdirSync(sess, { recursive: true }); writeFileSync(lock, "12345"); }
  for (const p of k.payloads) returns.push(ev.resolveTombstone(c.ws, "s1", payload(c, p)));
  const file = join(sess, "s1.json");
  let state = null;
  if (existsSync(file) && !lstatSync(file).isDirectory()) {
    const text = readFileSync(file, "utf8");
    try { state = JSON.parse(text); delete state.updatedAt; } catch { state = { raw: text }; }
  }
  out.resolve[k.id] = { returns, seed, state, sessions: existsSync(sess) && lstatSync(sess).isDirectory() ? readdirSync(sess).sort() : null };
}

out.directives.escalation = ev.escalationDirective();
out.directives.verifier = [];
{
  const c = fresh();
  for (let i = 0; i < 4; i++) {
    const res = ev.runSubagentStopGate(payload(c, { agent_type: "executor", agent_id: "a1", turn_id: "t1", last_assistant_message: "Done." }));
    out.directives.verifier.push(res === "" ? null : JSON.parse(res));
  }
}
process.stdout.write(JSON.stringify(out, null, 1) + "\n");
