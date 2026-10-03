// Records what the CXC v0.2.40 oracle's write path does, for the Go tests to replay (no Node at test time): ensureState,
// writeState, withSessionLock, appendLedger, appendInterviewEvent and readInterviewEvents of pabcd-state/src/state.ts, run
// against a fresh work directory per case. A case records the bytes of the file the oracle left, the directory listing, or
// the error code it threw. Date is frozen so every updatedAt is 2026-01-01T00:00:00.000Z; the process id, which names the
// temp file and fills the lock, is never recorded as a value.
// Recorded with Node v24.20.0 as
//   node record-writes.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> > oracle-writes.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync, readFileSync, readdirSync, existsSync, rmSync, statSync } from "node:fs";
import { join } from "node:path";
import { performance } from "node:perf_hooks";

const FROZEN = Date.parse("2026-01-01T00:00:00.000Z");
const RealDate = Date;
let tick = null; // set to 0, each new Date() reads one second after the one before
globalThis.Date = class extends RealDate {
  constructor(...a) { super(...(a.length ? a : [FROZEN + (tick === null ? 0 : 1000 * tick++)])); }
  static now() { return FROZEN; }
};
const [oracleDist, workRoot] = process.argv.slice(2);
const { writeState, ensureState, appendLedger, appendInterviewEvent, readInterviewEvents, withSessionLock, defaultState, readState } = await import(oracleDist + "/state.js");
const { defaultInterview } = await import(oracleDist + "/interview.js");

let n = 0;
const fresh = () => { const d = join(workRoot, "c" + n++); mkdirSync(d, { recursive: true }); return d; };
const sess = (cwd) => join(cwd, ".codexclaw", "sessions");
const readIf = (p) => (existsSync(p) ? readFileSync(p, "utf8") : null);
const listing = (dir) => (existsSync(dir) ? readdirSync(dir).sort() : null);
const attempt = (fn) => { try { return { returned: fn() }; } catch (e) { return { threw: e.code ?? e.name, ...(e instanceof TypeError ? { message: e.message } : {}) }; } };
const out = [];
const rec = (id, body) => out.push({ id, ...body });
const thrower = (code) => () => { throw Object.assign(new Error(code), { code }); };

// ensureState
{
  const cwd = fresh();
  const first = attempt(() => ensureState(cwd, "rec-s1"));
  const file = readIf(join(sess(cwd), "rec-s1.json"));
  const second = attempt(() => ensureState(cwd, "rec-s1"));
  rec("ensure_fresh", { first, file, second, unchanged: readIf(join(sess(cwd), "rec-s1.json")) === file, listing: listing(sess(cwd)), ignore: readIf(join(cwd, ".codexclaw", ".gitignore")) });
}
for (const id of ["  padded  ", "../unsafe/session", "\uC138\uC158", ""]) {
  const cwd = fresh();
  rec("ensure_noncanonical", { sessionId: id, ...attempt(() => ensureState(cwd, id)), stateDirCreated: existsSync(join(cwd, ".codexclaw")) });
}
for (const code of ["EPERM", "ENOTSUP", "EXDEV", "EEXIST", "EIO", "EACCES"]) {
  const cwd = fresh();
  rec("ensure_link_" + code, { ...attempt(() => ensureState(cwd, "rec-s1", thrower(code))), file: readIf(join(sess(cwd), "rec-s1.json")), listing: listing(sess(cwd)) });
}
{
  const cwd = fresh();
  mkdirSync(join(sess(cwd), "rec-s1.json"), { recursive: true });
  rec("ensure_final_is_directory", { ...attempt(() => ensureState(cwd, "rec-s1")), listing: listing(sess(cwd)) });
}
{
  // the fallback stringifies a second defaultState, so the file it publishes carries a later reading of the clock
  const cwd = fresh();
  tick = 0;
  const r = attempt(() => ensureState(cwd, "rec-s1", thrower("EPERM")));
  tick = null;
  rec("ensure_fallback_restamps", { ...r, file: readIf(join(sess(cwd), "rec-s1.json")) });
}

// writeState
const big = (k) => Array.from({ length: 60 }, (_, i) => k + i);
const tracker = (() => {
  const t = defaultInterview(3);
  t.contradictions = Array.from({ length: 60 }, (_, i) => ({ contradictionId: "c" + i, severity: "low", summary: "s" + i }));
  t.assumptions = Array.from({ length: 60 }, (_, i) => ({ id: "a" + i, text: "t" + i, recorded: true }));
  t.dimensions.goal = { level: "mid", known: big("k"), unknown: big("u"), confidence: 0.5 };
  t.scanRounds = 2; t.lastScanRoundId = 1;
  return t;
})();
const states = {
  write_default: defaultState("rec-s1"),
  write_phase_b: {
    ...defaultState("rec-s1", "my-slug"), phase: "B", flags: { interview: false, auditPassed: true, checkPassed: false }, injectedTurns: ["t1", "t2"],
    lastInjectedPhase: "B", orchestrationActive: true, stopBlockPhase: "B", stopBlockCount: 2, stopBlockTurnId: "turn-9", stopBlockCapNotified: true,
    loopArmSeen: true, idleEditNudges: 3, memoryWriteRequested: true, memoryWriteTurn: "t1", memoryWriteGrant: true, planUnit: "devlog/_plan/260101_x", planEpoch: "e1",
    phaseEntrySource: { kind: "resolved", commitSha: "abc", dirty: true, capturedAt: "2026-01-01T00:00:00Z", treeHash: "h", sourceRoot: "/ws" },
    unverifiedSubagents: [{ agentId: "a1", turnId: "t1", agentType: "executor", attempts: 3, receiptClaimed: "r", recordedAt: "2026-01-01T00:00:00Z", resolvable: true }],
  },
  write_stale_updated_at: { ...defaultState("rec-s1"), updatedAt: "1999-09-09T09:09:09.999Z", phase: "P", orchestrationActive: true },
  write_markup: defaultState("rec-s1", "<script>a&b</script> \u2028 \u2029 \u00e9 \u{1F600} \\u2028"),
  write_tracker_over_cap: { ...defaultState("rec-s1"), phase: "I", interview: tracker },
  write_noncanonical_id: defaultState("a/b"),
};
for (const [id, state] of Object.entries(states)) {
  const cwd = fresh();
  const r = attempt(() => writeState(cwd, state));
  rec(id, { state: JSON.parse(JSON.stringify(state)), ...r, listing: listing(sess(cwd)), files: Object.fromEntries((listing(sess(cwd)) ?? []).map((f) => [f, readIf(join(sess(cwd), f))])) });
}
{
  const cwd = fresh();
  writeState(cwd, { ...defaultState("a/b"), phase: "P" });
  writeState(cwd, { ...defaultState("a-b"), phase: "B" });
  rec("write_alias_ids", { listing: listing(sess(cwd)), phase: readState(cwd, "a/b").phase, file: readIf(join(sess(cwd), "a-b.json")) });
}
{
  const cwd = fresh();
  mkdirSync(join(sess(cwd), "rec-s1.json"), { recursive: true });
  rec("write_final_is_directory", { ...attempt(() => writeState(cwd, defaultState("rec-s1"))), listing: listing(sess(cwd)) });
}
{
  const cwd = fresh();
  mkdirSync(sess(cwd), { recursive: true });
  writeFileSync(join(sess(cwd), "rec-s1.json.12345.1767225600000.tmp"), "half");
  writeFileSync(join(sess(cwd), "rec-s1.json"), "garbage");
  const r = attempt(() => writeState(cwd, { ...defaultState("rec-s1"), phase: "P" }));
  rec("write_orphan_tmp_kept", { ...r, listing: listing(sess(cwd)), orphan: readIf(join(sess(cwd), "rec-s1.json.12345.1767225600000.tmp")), phase: readState(cwd, "rec-s1").phase });
}
{
  const cwd = fresh();
  writeFileSync(join(cwd, ".codexclaw"), "a file");
  rec("write_state_dir_is_file", { ...attempt(() => writeState(cwd, defaultState("rec-s1"))), dir: readIf(join(cwd, ".codexclaw")) });
}

// appendLedger: each entry is built in the key order of the oracle caller named in note
const ev = "planned <a&b> \u2028 \u00e9 \u{1F600}";
const trans = { ts: "t1", sessionId: "rec-s1", from: "P", to: "A", reason: "cli" };
const ledgers = {
  ledger_cli_transition: [{ ...trans, evidence: ev }, "orchestrate-cli.ts:1123"],
  ledger_no_evidence: [{ ...trans, from: "IDLE", to: "P" }, "orchestrate-cli.ts:1123 without an attest"],
  ledger_cli_override: [{ ...trans, from: "I", to: "P", actor: "agent", override: true, scanEvidence: { scanRounds: 0, highContradictionCount: 0 }, evidence: "skip" }, "orchestrate-cli.ts:655"],
  ledger_cli_dclose: [{ ...trans, from: "C", to: "IDLE", reason: "done", checkEpoch: "c1", closedWorkPhaseId: "wp1", evidence: "verified" }, "orchestrate-cli.ts:1040"],
  ledger_cli_dclose_null_key: [{ ...trans, from: "C", to: "IDLE", reason: "done", checkEpoch: null, closedWorkPhaseId: null, evidence: "verified" }, "orchestrate-cli.ts:899"],
  ledger_hook_dclose_with_evidence: [{ ...{ ...trans, from: "C", to: "IDLE", reason: "done", evidence: "verified" }, checkEpoch: "c1", closedWorkPhaseId: null }, "hook.ts:1147 (spreads the transition row)"],
  ledger_hook_dclose: [{ ...{ ...trans, from: "C", to: "IDLE", reason: "done" }, checkEpoch: "c1", closedWorkPhaseId: "wp1" }, "hook.ts:1335 without evidence"],
  ledger_evidence_resolve: [{ ...trans, from: "B", to: "B", reason: "evidence resolve: agent=a1", evidence: "r.json", actor: "agent", override: false }, "evidence-cli.ts:88"],
  ledger_chat_transition: [{ ...trans, reason: "chat", actor: "human", evidence: "ok" }, "orchestrate-apply.ts:189"],
  ledger_chat_override: [{ ...trans, from: "I", to: "P", reason: "chat", actor: "human", override: true, scanEvidence: { scanRounds: 2, highContradictionCount: 1 }, evidence: "go" }, "orchestrate-apply.ts:150"],
  ledger_reset: [{ ts: "t1", sessionId: "rec-s1", from: "B", to: "IDLE", reason: "reset" }, "orchestrate-apply.ts:93"],
  ledger_from_null: [{ ...trans, from: null }, "LedgerEntry.from is Phase | null"],
};
for (const [id, [entry, note]] of Object.entries(ledgers)) {
  const cwd = fresh();
  appendLedger(cwd, entry);
  rec(id, { note, entry, file: readIf(join(cwd, ".codexclaw", "ledger.jsonl")) });
}
{
  const cwd = fresh();
  appendLedger(cwd, ledgers.ledger_reset[0]);
  appendLedger(cwd, ledgers.ledger_no_evidence[0]);
  rec("ledger_two_rows", { file: readIf(join(cwd, ".codexclaw", "ledger.jsonl")) });
}

// appendInterviewEvent
{
  const cwd = fresh();
  // scan-cli.ts:325 builds the event in this key order; a later key in extra keeps its place, map comes last
  const event = (kind, extra = {}) => ({ ts: "t1", sessionId: "rec-s1", event: kind, roundId: 1, contradictionCount: 3, highContradictionCount: 1, ...extra });
  appendInterviewEvent(cwd, event("scan_started"));
  appendInterviewEvent(cwd, event("scan_completed", { roundId: 2, map: { "q-b": "goal", "q-a": "constraint" } }));
  appendInterviewEvent(cwd, event("rescan_completed", { sessionId: "a/b", roundId: 3 }));
  const dir = join(cwd, ".codexclaw", "interviews");
  rec("interview_events", { listing: listing(dir), file: readIf(join(dir, "rec-s1.jsonl")), aliasFile: readIf(join(dir, "a-b.jsonl")), events: readInterviewEvents(cwd, "rec-s1") });
}

// readInterviewEvents over a ledger shared with Q/A rows and damaged lines
{
  const cwd = fresh();
  const dir = join(cwd, ".codexclaw", "interviews");
  mkdirSync(dir, { recursive: true });
  const rows = [
    '{"ts":"t1","sessionId":"iv","event":"scan_started","roundId":1,"contradictionCount":3,"highContradictionCount":1}',
    '{"ts":"t2","sessionId":"iv","turnId":"t1","event":"question_asked","questionId":"q1","eventId":"e1","question":"Goal?"}',
    "not json",
    '  {"ts":"t3","sessionId":"iv","event":"scan_completed","roundId":1,"contradictionCount":0}',
    "[]",
    '{"event":"scan_completed","roundId":"1","contradictionCount":0}',
    '{"event":"scan_completed","roundId":2,"contradictionCount":"0"}',
    '{"ts":"t4","sessionId":"iv","event":"rescan_completed","roundId":2,"contradictionCount":1,"highContradictionCount":0,"extra":true,"map":{"q-b":"goal","q-a":"constraint"}}\r',
    "\uFEFF" + '{"event":"scan_started","roundId":3,"contradictionCount":0,"highContradictionCount":0}',
    '{"event":"scan_bogus","roundId":4,"contradictionCount":0}',
    '{"event":"scan_started","roundId":5,"contradictionCount":0} {"x":1}',
    "",
    '{"event":"answer_recorded","roundId":6,"contradictionCount":0}',
  ];
  const input = rows.join("\n") + "\n";
  writeFileSync(join(dir, "iv.jsonl"), input);
  rec("interview_read_mixed", { input, events: readInterviewEvents(cwd, "iv") });
  rec("interview_read_missing", { events: readInterviewEvents(cwd, "nope") });
  mkdirSync(join(dir, "dir.jsonl"));
  rec("interview_read_directory", { events: readInterviewEvents(cwd, "dir") });
}

// withSessionLock
{
  const cwd = fresh();
  const lock = join(sess(cwd), "rec-s1.json.lock");
  const inside = {};
  const r = attempt(() => withSessionLock(cwd, "rec-s1", () => { inside.holdsPid = readFileSync(lock, "utf8") === String(process.pid); return "result"; }));
  rec("lock_runs_and_releases", { ...r, ...inside, lockAfter: existsSync(lock) });
}
{
  const cwd = fresh();
  const lock = join(sess(cwd), "rec-s1.json.lock");
  const r = attempt(() => withSessionLock(cwd, "rec-s1", () => { throw Object.assign(new Error("boom"), { code: "BOOM" }); }));
  rec("lock_released_when_fn_throws", { ...r, lockAfter: existsSync(lock) });
}
{
  const cwd = fresh();
  mkdirSync(sess(cwd), { recursive: true });
  const lock = join(sess(cwd), "rec-s1.json.lock");
  writeFileSync(lock, "424242");
  let entered = false;
  const t0 = performance.now();
  const r = attempt(() => withSessionLock(cwd, "rec-s1", () => { entered = true; }));
  const ms = performance.now() - t0;
  rec("lock_held_exhausts", { ...r, entered, lockAfter: readIf(lock), waitedAtLeast240ms: ms >= 240, waitedUnder2s: ms < 2000 });
}
{
  const cwd = fresh();
  const lock = join(sess(cwd), "rec-s1.json.lock");
  const r = attempt(() => withSessionLock(cwd, "rec-s1", () => { writeFileSync(lock, "another holder"); }));
  rec("lock_release_removes_a_foreign_lock", { ...r, lockAfter: existsSync(lock) });
}
{
  const cwd = fresh();
  mkdirSync(sess(cwd), { recursive: true });
  writeFileSync(join(sess(cwd), "x.json.lock"), "1");
  rec("lock_key_is_sanitised", { ...attempt(() => withSessionLock(cwd, "x", () => 1)), alias: attempt(() => withSessionLock(cwd, "x/", () => 1)) });
}


// ---- the oracle's real behaviour under the conditions the Go tests must meet ----
const octal = (p) => (statSync(p).mode & 0o777).toString(8);
const script = (body) => ["--input-type=module", "-e", 'import { writeState, ensureState, withSessionLock, defaultState } from ' + JSON.stringify(oracleDist + "/state.js") + ";\n" + body];
const child = (args, opts = {}) => new Promise((resolve) => {
  const c = spawn(process.execPath, args, { stdio: ["ignore", "pipe", "ignore"], ...opts });
  let stdout = "";
  c.stdout.on("data", (d) => (stdout += d));
  c.on("exit", (code, signal) => resolve({ code, signal, stdout }));
});

// file modes under two umasks (writeFileSync and appendFileSync create 0666 less umask)
for (const mask of [0o022, 0o077]) {
  const old = process.umask(mask);
  const cwd = fresh();
  ensureState(cwd, "ens");
  writeState(cwd, defaultState("wr"));
  appendLedger(cwd, { ts: "t", sessionId: "wr", from: null, to: "P", reason: "x" });
  appendInterviewEvent(cwd, { ts: "t", sessionId: "wr", event: "scan_started", roundId: 1, contradictionCount: 0, highContradictionCount: 0 });
  let lockMode;
  withSessionLock(cwd, "wr", () => { lockMode = octal(join(sess(cwd), "wr.json.lock")); });
  rec("modes_umask_" + mask.toString(8).padStart(3, "0"), {
    ensure: octal(join(sess(cwd), "ens.json")), write: octal(join(sess(cwd), "wr.json")), ledger: octal(join(cwd, ".codexclaw", "ledger.jsonl")),
    interview: octal(join(cwd, ".codexclaw", "interviews", "wr.jsonl")), lock: lockMode,
  });
  process.umask(old);
}

// rmSync(tmp, { force: true }) in ensureState's finally refuses a directory, empty or not, and its error replaces the result
for (const [empty, how] of [[true, "ok"], [false, "ok"], [true, "eexist"], [false, "eexist"]]) {
  const cwd = fresh();
  const swap = (tmp) => { rmSync(tmp); mkdirSync(tmp); if (!empty) writeFileSync(join(tmp, "x"), "1"); if (how === "eexist") throw Object.assign(new Error("EEXIST"), { code: "EEXIST" }); };
  const r = attempt(() => ensureState(cwd, "rec-s1", (tmp) => swap(tmp)));
  const left = (listing(sess(cwd)) ?? []).filter((f) => f.endsWith(".tmp"));
  rec("ensure_tmp_replaced_by_directory_" + (empty ? "empty" : "full") + "_link_" + how, { ...r, tmpLeft: left.length, final: listing(sess(cwd)).includes("rec-s1.json") });
}
// the lock release swallows the same refusal and leaves the directory
for (const empty of [true, false]) {
  const cwd = fresh();
  const lock = join(sess(cwd), "rec-s1.json.lock");
  const r = attempt(() => withSessionLock(cwd, "rec-s1", () => { rmSync(lock); mkdirSync(lock); if (!empty) writeFileSync(join(lock, "x"), "1"); return "result"; }));
  rec("lock_release_over_a_directory_" + (empty ? "empty" : "full"), { ...r, lockIsDirectory: statSync(lock).isDirectory() });
}

// map keys: JS orders canonical array indexes first, ascending, then the others in insertion order
{
  const cwd = fresh();
  appendInterviewEvent(cwd, { ts: "t1", sessionId: "rec-s1", event: "scan_completed", roundId: 1, contradictionCount: 0, highContradictionCount: 0, map: { "10": "a", "2": "b", x: "c", "1": "d", "01": "e", "4294967295": "f", y: "g" } });
  const dir = join(cwd, ".codexclaw", "interviews");
  rec("interview_map_key_order", { file: readIf(join(dir, "rec-s1.jsonl")), events: readInterviewEvents(cwd, "rec-s1") });
  const rawInput = '{"event":"scan_started","roundId":1,"contradictionCount":0,"map":{"10":"a","2":"b","x":"c","1":"d","x":"z"}}\n';
  writeFileSync(join(dir, "raw.jsonl"), rawInput);
  rec("interview_read_map_key_order", { input: rawInput, events: readInterviewEvents(cwd, "raw") });
}

// real processes: concurrent writers, a creation race, lock contention, and a kill between the temp write and the rename
{
  let oneDocument = 0, tmpLeft = 0, errors = 0;
  for (let round = 0; round < 10; round++) {
    const cwd = fresh();
    const runs = await Promise.all(["P", "A", "B", "C"].map((phase) => child(script('writeState(process.argv[1], { ...defaultState("conc"), phase: "' + phase + '", slug: "slug-' + phase + '" });') .concat(cwd))));
    errors += runs.filter((r) => r.code !== 0).length;
    const file = JSON.parse(readFileSync(join(sess(cwd), "conc.json"), "utf8"));
    if (file.slug === "slug-" + file.phase && file.sessionId === "conc") oneDocument++;
    tmpLeft += (listing(sess(cwd)) ?? []).filter((f) => f.endsWith(".tmp")).length;
  }
  rec("concurrent_writers", { rounds: 10, childErrors: errors, roundsWithOneCompleteDocument: oneDocument, tmpLeft });
}
{
  let created = [];
  for (let round = 0; round < 5; round++) {
    const cwd = fresh();
    const runs = await Promise.all([0, 1, 2, 3].map(() => child(script('process.stdout.write(String(ensureState(process.argv[1], "conc")));').concat(cwd))));
    created.push(runs.filter((r) => r.stdout === "true").length);
    if (runs.some((r) => r.code !== 0)) created.push("child failed");
    if (!existsSync(join(cwd, ".codexclaw", ".gitignore"))) created.push("no ignore file");
  }
  rec("concurrent_ensure_state", { createdPerRound: created });
}
{
  // Handshake, not timing. A logs "A-in" once it holds the lock and keeps it until B logs "B-got-EEXIST"; B wraps the oracle's writeFileSync
  // in its own process and logs that line when the exclusive create of the lock file really fails with EEXIST, so A leaves (20 ms
  // later, inside B's retry budget of about 250 ms) only after B has met the held lock, and B enters after "A-out". A wait that is
  // never answered ends the child with exit code 2.
  const cwd = fresh();
  const log = join(cwd, "log");
  const lockFile = join(sess(cwd), "conc.json.lock");
  const wait = "const wait = (file, text) => { for (let i = 0; i < 1500; i++) { if (fs.existsSync(file) && fs.readFileSync(file, 'utf8').includes(text)) return; Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 2); } process.exit(2); };";
  const holder = ['import fs from "node:fs";', wait, 'const { withSessionLock } = await import(' + JSON.stringify(oracleDist + "/state.js") + ');',
    'withSessionLock(process.argv[1], "conc", () => { fs.appendFileSync(process.argv[2], "A-in\\n"); wait(process.argv[2], "B-got-EEXIST"); Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 20); fs.appendFileSync(process.argv[2], "A-out\\n"); });'].join("\n");
  const waiter = ['import fs from "node:fs"; import { syncBuiltinESMExports } from "node:module";', wait,
    'const realWrite = fs.writeFileSync; fs.writeFileSync = (path, data, options) => { try { return realWrite(path, data, options); } catch (e) { if (path === process.argv[3] && e.code === "EEXIST") fs.appendFileSync(process.argv[2], "B-got-EEXIST\\n"); throw e; } }; syncBuiltinESMExports();',
    'const { withSessionLock } = await import(' + JSON.stringify(oracleDist + "/state.js") + ');',
    'wait(process.argv[2], "A-in"); withSessionLock(process.argv[1], "conc", () => { fs.appendFileSync(process.argv[2], "B-in\\n"); });'].join("\n");
  const [ra, rb] = await Promise.all([child(["--input-type=module", "-e", holder, cwd, log]), child(["--input-type=module", "-e", waiter, cwd, log, lockFile])]);
  // B retries until A leaves, so it meets EEXIST once or several times (also just after A-out, before the unlink): the lines are left
  // out of the order and recorded as "met EEXIST" and "before A-out"
  const lines = readFileSync(log, "utf8").trim().split("\n");
  rec("lock_contention_between_processes", { order: lines.filter((l) => l !== "B-got-EEXIST"), metEEXIST: lines.includes("B-got-EEXIST"), metEEXISTBeforeAOut: lines.indexOf("B-got-EEXIST") > 0 && lines.indexOf("B-got-EEXIST") < lines.indexOf("A-out"), exits: [ra.code, rb.code], lockAfter: existsSync(lockFile) });
}
{
  const cwd = fresh();
  writeState(cwd, { ...defaultState("rec-s1"), phase: "B" });
  const kill = ['import fs from "node:fs"; import { syncBuiltinESMExports } from "node:module"; fs.renameSync = () => process.kill(process.pid, "SIGKILL"); syncBuiltinESMExports();',
    'const { writeState, defaultState } = await import(' + JSON.stringify(oracleDist + "/state.js") + '); writeState(process.argv[1], { ...defaultState("rec-s1"), phase: "P" });'].join("\n");
  const r = await child(["--input-type=module", "-e", kill, cwd]);
  const tmps = (listing(sess(cwd)) ?? []).filter((f) => f.endsWith(".tmp"));
  const before = { signal: r.signal, listingCount: listing(sess(cwd)).length, tmpCount: tmps.length, tmpHoldsNewDocument: tmps.length === 1 && JSON.parse(readFileSync(join(sess(cwd), tmps[0]), "utf8")).phase === "P", phase: readState(cwd, "rec-s1").phase };
  writeState(cwd, { ...defaultState("rec-s1"), phase: "C" });
  rec("killed_between_temp_write_and_rename", { ...before, afterNextWrite: { phase: readState(cwd, "rec-s1").phase, tmpCount: (listing(sess(cwd)) ?? []).filter((f) => f.endsWith(".tmp")).length } });
}

process.stdout.write(JSON.stringify(out, null, 1) + "\n");
