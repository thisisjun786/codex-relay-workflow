// Records what the CXC v0.2.40 oracle's evidence attempts, receipt check and tombstone writer answer for the cases of
// cases.json, for the Go tests to replay from oracle-evidence.json (no Node at test time): extractReceiptPath,
// attemptsPath (through the file writeAttempts leaves), readAttempts, writeAttempts, clearAttempts,
// transcriptHasContextPressure, hasValidReceipt, hasTombstone and recordTombstone of
// pabcd-state/src/subagent-evidence.ts. Every case runs in a fresh work directory under the work root; "{S}" in a
// case is the state directory name (.codexclaw in the oracle, .crw in the port), "<CWD>" and "<OUT>" the case's
// workspace and an outside directory. Date is frozen so recordedAt and updatedAt are 2026-01-01T00:00:00.000Z.
// Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> cases.json > oracle-evidence.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdirSync, writeFileSync, readFileSync, readdirSync, existsSync, rmSync, symlinkSync, linkSync, chmodSync, lstatSync } from "node:fs";
import { execFileSync, spawn } from "node:child_process";
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
const body = (o) => (o.b64 !== undefined ? Buffer.from(o.b64, "base64") : o.fill !== undefined ? Buffer.from(o.fill.repeat(o.times) + o.tail) : Buffer.from(o.text ?? ""));
const deep = (o) => "[".repeat(o.deep) + (o.open ? "" : "]".repeat(o.deep)) + (o.tail ?? "");
const parent = (p) => mkdirSync(dirname(p), { recursive: true });
const isDir = (p) => existsSync(p) && lstatSync(p).isDirectory();
const listing = (dir) => (isDir(dir) ? readdirSync(dir).sort().map((f) => f.replace(/\.\d+\.\d+\.tmp$/, ".<PID>.<MS>.tmp")) : null);
const ls = (c) => join(c.ws, S, "evidence-attempts");
const counterFile = (c, s, a, t) => { ev.writeAttempts(c.ws, s, a, 0, t); const f = readdirSync(ls(c)).filter((x) => x.endsWith(".json")); return join(ls(c), f[f.length - 1]); };

const out = { gated: { types: [...ev.GATED_AGENT_TYPES].sort(), max: ev.MAX_ATTEMPTS }, extract: {}, names: {}, counter: {}, pressure: {}, receipt: {}, writes: {}, tombstone: {} };
for (const k of cases.extract) out.extract[k.id] = ev.extractReceiptPath(k.message);
for (const k of cases.names) {
  const c = fresh();
  ev.writeAttempts(c.ws, k.session, k.agent, 2, k.turn);
  const files = readdirSync(ls(c));
  out.names[k.id] = { files, content: files.map((f) => readFileSync(join(ls(c), f), "utf8")) };
}
for (const k of cases.counter) {
  const c = fresh();
  const f = counterFile(c, k.session ?? "s1", k.agent ?? "a1", k.turn ?? "");
  rmSync(f);
  if (k.dir) mkdirSync(f);
  else if (k.raw !== null) writeFileSync(f, typeof k.raw === "string" ? k.raw : deep(k.raw));
  out.counter[k.id] = ev.readAttempts(c.ws, "s1", "a1", "");
}
for (const k of cases.pressure) {
  const c = fresh(), p = join(c.ws, "child.jsonl");
  if (k.dir) mkdirSync(p);
  else if (!k.missing) writeFileSync(p, body(k));
  out.pressure[k.id] = ev.transcriptHasContextPressure(p);
}
for (const k of cases.receipt) {
  const c = fresh();
  for (const o of k.setup) {
    const p = at(c, o.file ?? o.dir ?? o.symlink ?? o.fifo ?? o.chmod ?? o.hardlink);
    if (o.dir) mkdirSync(p, { recursive: true });
    else if (o.file !== undefined) { parent(p); writeFileSync(p, body(o)); }
    else if (o.symlink) { parent(p); symlinkSync(sub(o.to, c), p); }
    else if (o.hardlink) { parent(p); linkSync(sub(o.to, c), p); }
    else if (o.fifo) { parent(p); execFileSync("mkfifo", [p]); }
    else if (o.chmod) chmodSync(p, parseInt(o.mode, 8));
  }
  out.receipt[k.id] = ev.hasValidReceipt(c.ws, sub(k.claim, c));
}
for (const k of cases.writes) {
  const c = fresh(), returns = [];
  if (k.pre === "state_dir_is_file") writeFileSync(join(c.ws, S), "x");
  if (k.pre === "attempts_dir_is_file") { mkdirSync(join(c.ws, S)); writeFileSync(ls(c), "x"); }
  if (k.pre === "counter_is_directory") { const f = counterFile(c, "s1", "a1", ""); rmSync(f); mkdirSync(f); }
  for (const o of k.ops) returns.push(o.write !== undefined ? ev.writeAttempts(c.ws, "s1", "a1", o.write, o.turn ?? "") : (ev.clearAttempts(c.ws, "s1", "a1", o.turn ?? ""), null));
  const files = listing(ls(c));
  const dirOrFile = (f) => (lstatSync(join(ls(c), f)).isDirectory() ? "<dir>" : readFileSync(join(ls(c), f), "utf8"));
  out.writes[k.id] = { returns, files, content: files ? readdirSync(ls(c)).sort().map(dirOrFile) : null };
}
const payload = (c, p) => ({ hook_event_name: "SubagentStop", session_id: "s1", cwd: c.ws, ...p });
for (const k of cases.tombstone) {
  for (let attempt = 0; ; attempt++) {
    const c = fresh(), sess = join(c.ws, S, "sessions"), lock = join(sess, "s1.json.lock"), returns = [];
    let seed = null;
    if (k.seedOverrides) { st.writeState(c.ws, { ...st.defaultState("s1"), ...k.seedOverrides }); seed = readFileSync(join(sess, "s1.json"), "utf8"); }
    if (k.stateRaw !== undefined) { mkdirSync(sess, { recursive: true }); writeFileSync(join(sess, "s1.json"), k.stateRaw); }
    if (k.stateDirIsFile) writeFileSync(join(c.ws, S), "x");
    if (k.sessionsIsFile) { mkdirSync(join(c.ws, S)); writeFileSync(sess, "x"); }
    if (k.lock) { mkdirSync(sess, { recursive: true }); writeFileSync(lock, "12345"); }
    if (k.lock === "sentinel_window") spawn("sh", ["-c", "sleep 0.4; rm -f '" + lock + "'"], { detached: true, stdio: "ignore" }).unref();
    for (const o of k.ops) returns.push(o.op === "record" ? ev.recordTombstone(c.ws, "s1", payload(c, o.p), o.attempts) : ev.hasTombstone(c.ws, "s1", payload(c, o.p)));
    if (k.lock === "sentinel_window") sleep(300);
    const file = join(sess, "s1.json");
    let state = null;
    if (existsSync(file) && !lstatSync(file).isDirectory()) {
      const text = readFileSync(file, "utf8");
      try { state = JSON.parse(text); delete state.updatedAt; } catch { state = { raw: text }; }
    }
    const mdir = join(c.ws, S, "evidence-unrecordable");
    const markers = existsSync(mdir) ? readdirSync(mdir).sort().map((f) => { const m = JSON.parse(readFileSync(join(mdir, f), "utf8")); return { sessionId: m.sessionId, agentId: m.agentId }; }) : [];
    if (k.lock === "sentinel_window" && !(state && state.unverifiedCorrupt === true) && attempt < 3) continue;
    if (k.lock === "sentinel_window" && !(state && state.unverifiedCorrupt === true)) throw new Error("sentinel tier did not occur in " + k.id);
    out.tombstone[k.id] = { returns, seed, state, markers };
    break;
  }
}
process.stdout.write(JSON.stringify(out, null, 1) + "\n");
