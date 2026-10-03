// Records what the CXC v0.2.40 oracle's applyHumanTransition answers over the grid below; apply_test.go replays oracle-apply.json
// (no Node at test time). Recorded with Node v24.20.0 as
//   node record-apply-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <out dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d). The clock is frozen, so a
// ledger row's ts is FROZEN. A state is recorded as the top-level keys the call changed against its seed, each with the oracle's
// JSON value, and a ledger row as the oracle's JSON.stringify. The seed and the trackers are a copy of record-oracle.mjs's, by rule
// and value-identical: every field of the seed holds a distinct value, so a port that clears or keeps the wrong one shows. An
// attestation is a plain object, so a did of one space reaches the function untouched. A row is [from, verb, flags, tracker,
// attest, reset case, ok, reason, changed keys, ledger row, control, noop]; the constructor verb is the function Object.
import { mkdirSync, writeFileSync } from "node:fs";
const [dist, outDir] = process.argv.slice(2);
const FROZEN = "2026-01-01T00:00:00.000Z", RealDate = Date;
globalThis.Date = class extends RealDate { constructor(...a) { super(...(a.length ? a : [FROZEN])); } };
const { applyHumanTransition } = await import(dist + "/orchestrate-apply.js");
mkdirSync(outDir, { recursive: true });
const table = () => { const list = [], seen = new Map(); return [list, (s) => { if (!seen.has(s)) { seen.set(s, list.length); list.push(s); } return seen.get(s); }]; };
const [texts, T] = table(), [diffs, D] = table(), [ledgers, L] = table();
const dim = (level) => ({ level, known: ["k"], unknown: [], confidence: 1 });
const tracker = (o = {}) => ({ roundId: 1, dimensions: { goal: dim(o.level ?? "max"), constraint: dim("max"), success: dim("max"), ontology: dim("max") }, contradictions: o.contradictions ?? [], assumptions: o.assumptions ?? [], autoResolveCount: 0, consecutiveAutoResolves: 0, scanRounds: o.scan ?? 1, lastScanRoundId: 1 });
const trackers = { null: null, ready: tracker(), noscan: tracker({ scan: 0 }), negscan: tracker({ scan: -1 }), manyscans: tracker({ scan: 7 }), mid: tracker({ level: "mid" }), high: tracker({ level: "high" }), low: tracker({ level: "low", scan: 0 }),
  contradiction: tracker({ contradictions: [{ contradictionId: "c1", severity: "high", summary: "s" }, { contradictionId: "c2", severity: "low", summary: "s" }, { contradictionId: "c3", severity: "high", summary: "s" }] }),
  unrecorded: tracker({ assumptions: [{ id: "a1", text: "t", recorded: false }] }) };
const flagsOf = (n) => ({ interview: !!(n & 1), auditPassed: !!(n & 2), checkPassed: !!(n & 4) });
const seed = (phase, n, interview, over = {}) => ({ phase, sessionId: "rec-s1", slug: "seed", updatedAt: FROZEN, flags: flagsOf(n), supersededBy: "s2", injectedTurns: ["x"], lastInjectedPhase: "C", orchestrationActive: true, interview,
  stopBlockPhase: "B", stopBlockCount: 2, stopBlockWorkPhaseId: "wp1", stopMetricCursor: 3, stopBlockTotal: 4, stopBlockTurnId: "t9", stopBlockCapNotified: true, loopArmSeen: true, idleEditNudges: 5, memoryWriteRequested: true, memoryWriteTurn: "t8", memoryWriteGrant: true,
  unverifiedSubagents: [{ agentId: "a1", turnId: "t1", agentType: "worker", attempts: 2, receiptClaimed: "r.json", recordedAt: FROZEN, resolvable: true }], unverifiedCorrupt: true,
  phaseEntrySource: { kind: "resolved", commitSha: "abc", dirty: true, capturedAt: FROZEN, treeHash: "def", sourceRoot: "/r" }, boundSourceRoot: "/r", planUnit: "devlog/_plan/u", planEpoch: "e1", checkEpoch: "c1",
  dcloseRecovery: { sessionId: "rec-s1", checkEpoch: "c1", closedWorkPhaseId: "wp1", nextWorkPhaseId: "wp2" }, ...over });
const PHASES = ["IDLE", "I", "P", "A", "B", "C", "D"], FLAGS = [0, 2, 5, 7], VERBS = ["I", "P", "A", "B", "C", "D", "status", "reset", "IDLE", "Z", "constructor"];
const ATTESTS = [null, { did: "chat evidence" }, { did: "accept the risk", override: true }, { override: true }, { did: "" }, { did: "x", override: false }, { did: " ", override: true }];
const rows = [];
const record = (from, verb, n, tk, ai, over = {}) => {
  const s = seed(from, n, trackers[tk], over), before = JSON.stringify(s), a = ATTESTS[ai] === null ? null : { from: "I", to: "P", ...ATTESTS[ai] };
  const r = applyHumanTransition(s, verb === "constructor" ? Object : verb, a);
  if (JSON.stringify(s) !== before) throw new Error("the oracle changed its input");
  const changed = r.state === undefined ? -1 : D(JSON.stringify(Object.fromEntries([...new Set([...Object.keys(s), ...Object.keys(r.state)])].filter((k) => JSON.stringify(s[k]) !== JSON.stringify(r.state[k])).map((k) => [k, r.state[k] === undefined ? null : r.state[k]]))));
  rows.push([from, verb, n, tk, ai, over.id ?? "", +r.ok, r.reason === undefined ? -1 : T(r.reason), changed, r.ledger === undefined ? -1 : L(JSON.stringify(r.ledger)), r.control ?? "", +(r.noop === true)]);
};
for (const from of PHASES) for (const n of FLAGS) for (const verb of VERBS) {
  if (from === "I" && verb === "P") { for (const tk of Object.keys(trackers)) for (let ai = 0; ai < ATTESTS.length; ai++) record(from, verb, n, tk, ai); continue; }
  for (const ai of [0, 1, 2]) record(from, verb, n, "ready", ai);
}
// reset from IDLE: a no-op only when neither a check epoch nor a D-close marker is left (the seed holds both)
for (const [id, over] of [["bare", { checkEpoch: null, dcloseRecovery: null }], ["epoch", { dcloseRecovery: null }], ["marker", { checkEpoch: null }]]) record("IDLE", "reset", 0, "ready", 0, { ...over, id });
writeFileSync(outDir + "/oracle-apply.json", JSON.stringify({ seed: seed("IDLE", 0, null), trackers, attests: ATTESTS, texts, diffs, ledgers, rows }) + "\n");

