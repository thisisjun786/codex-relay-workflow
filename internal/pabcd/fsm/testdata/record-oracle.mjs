// Records what the CXC v0.2.40 oracle's fsm.ts and orchestrate-grammar.ts answer over the grids below; the Go tests replay
// oracle-fsm.json and oracle-grammar.json (no Node at test time). Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <out dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d). A state is the oracle's
// JSON.stringify(state), the seed written in the key order of the Go State struct, so the Go test compares state.Encode with it.
// A chat prompt is kept in the oracle's spelling ("o") beside the crw spelling ("c") the Go side runs (name-substitution R1, R10, R27).
import { mkdirSync, writeFileSync } from "node:fs";
const [dist, outDir] = process.argv.slice(2);
const F = await import(dist + "/fsm.js");
const G = await import(dist + "/orchestrate-grammar.js");
const A = await import(dist + "/attest.js");
mkdirSync(outDir, { recursive: true });
const table = () => { const list = [], seen = new Map(); return [list, (s) => { if (!seen.has(s)) { seen.set(s, list.length); list.push(s); } return seen.get(s); }]; };
const [texts, T] = table(), [states, S] = table(), [inputs, I] = table();
const PHASES = ["IDLE", "I", "P", "A", "B", "C", "D"], ODD = ["Z", "constructor", "__proto__", "toString"];
const FLAGS = [0, 1, 2, 3, 4, 5, 6, 7], flagsOf = (n) => ({ interview: !!(n & 1), auditPassed: !!(n & 2), checkPassed: !!(n & 4) });
const dim = (level) => ({ level, known: ["k"], unknown: [], confidence: 1 });
const tracker = (o = {}) => ({ roundId: 1, dimensions: { goal: dim(o.level ?? "max"), constraint: dim("max"), success: dim("max"), ontology: dim("max") }, contradictions: o.contradictions ?? [], assumptions: o.assumptions ?? [], autoResolveCount: 0, consecutiveAutoResolves: 0, scanRounds: o.scan ?? 1, lastScanRoundId: 1 });
const trackers = { null: null, ready: tracker(), noscan: tracker({ scan: 0 }), mid: tracker({ level: "mid" }), high: tracker({ level: "high" }), low: tracker({ level: "low", scan: 0 }),
  contradiction: tracker({ contradictions: [{ contradictionId: "c1", severity: "high", summary: "s" }] }), unrecorded: tracker({ assumptions: [{ id: "a1", text: "t", recorded: false }] }), recorded: tracker({ assumptions: [{ id: "a1", text: "t", recorded: true }] }) };
// every field of a persisted state holds a distinct value, so a port that clears or keeps the wrong one shows
const seed = (phase, n, interview = null) => ({ phase, sessionId: "rec-s1", slug: "seed", updatedAt: "2026-01-01T00:00:00.000Z", flags: flagsOf(n), supersededBy: "s2", injectedTurns: ["x"], lastInjectedPhase: "C", orchestrationActive: true, interview,
  stopBlockPhase: "B", stopBlockCount: 2, stopBlockWorkPhaseId: "wp1", stopMetricCursor: 3, stopBlockTotal: 4, stopBlockTurnId: "t9", stopBlockCapNotified: true, loopArmSeen: true, idleEditNudges: 5, memoryWriteRequested: true, memoryWriteTurn: "t8", memoryWriteGrant: true,
  unverifiedSubagents: [{ agentId: "a1", turnId: "t1", agentType: "worker", attempts: 2, receiptClaimed: "r.json", recordedAt: "2026-01-01T00:00:00.000Z", resolvable: true }], unverifiedCorrupt: true,
  phaseEntrySource: { kind: "resolved", commitSha: "abc", dirty: true, capturedAt: "2026-01-01T00:00:00.000Z", treeHash: "def", sourceRoot: "/r" }, boundSourceRoot: "/r", planUnit: "devlog/_plan/u", planEpoch: "e1", checkEpoch: "c1",
  dcloseRecovery: { sessionId: "rec-s1", checkEpoch: "c1", closedWorkPhaseId: "wp1", nextWorkPhaseId: "wp2" } });
const run = (f) => { try { return f(); } catch { return { threw: true }; } };
const verdict = (r) => (r.threw ? 2 : r.ok ? 1 : 0);
const edges = [...PHASES, ...ODD].flatMap((f) => [...PHASES, "Z", "constructor"].map((t) => [f, t]));
const canEnter = edges.flatMap(([f, t]) => FLAGS.map((n) => { const r = run(() => F.canEnter(t, seed(f, n))); return [f, t, n, verdict(r), r.reason === undefined ? -1 : T(r.reason)]; }));
const legal = edges.map(([f, t]) => [f, t, run(() => ({ ok: F.isLegalEdge(f, t) })).threw ? 2 : F.isLegalEdge(f, t) ? 1 : 0]);
const next = [...PHASES, ...ODD].map((p) => [p, F.nextPhase(seed(p, 0)) ?? ""]);
const gates = PHASES.flatMap((p) => FLAGS.map((n) => { const s = seed(p, n); return [p, n, +F.isAuditGateOpen(s), +F.isBuildGateOpen(s), +F.isDone(s), +F.isIdle(s)]; }));
const attests = (f, t) => [null, { from: f, to: t, did: "implemented the audited plan" }, { from: f, to: t, did: "tbd" }, { from: t, to: f, did: "real work" }, { from: f, to: t, did: "audited", auditOutput: "reviewer verdict: GO; no blockers", auditVerdict: "pass" },
  { from: f, to: t, did: "ran tests", checkOutput: "77 pass", exitCode: 0 }, { from: f, to: t, did: "ran tests", checkOutput: "x", exitCode: 1 }].map((a) => (a === null ? null : JSON.stringify(a)));
const transitions = [];
for (const [f, t] of edges.filter(([f, t]) => (PHASES.includes(f) && PHASES.includes(t)) || (!PHASES.includes(f) && ["I", "P", "IDLE"].includes(t)))) for (const raw of attests(f, t).slice(0, PHASES.includes(f) ? 7 : 2)) for (const n of FLAGS) {
  const r = run(() => F.transition(seed(f, n, trackers.recorded), t, raw === null ? null : A.coerceAttest(JSON.parse(raw))));
  transitions.push([f, t, n, raw === null ? -1 : I(raw), verdict(r), r.reason === undefined ? -1 : T(r.reason), r.state === undefined ? -1 : S(JSON.stringify(r.state))]);
}
const derive = Object.entries(trackers).flatMap(([name, tr]) => [7, 0].map((n) => [name, n, S(JSON.stringify(F.deriveInterviewFlag(seed("I", n, tr))))]));
writeFileSync(outDir + "/oracle-fsm.json", JSON.stringify({ seed: seed("IDLE", 0), trackers, order: F.ORDER, validTransitions: F.VALID_TRANSITIONS, texts, states, inputs, canEnter, legal, next, gates, transitions, derive }) + "\n");

// ---- chat grammar ----
const PRE = [["", ""], ["/", "/"], ["$codexclaw:cxc-", "$crw:crw-"], ["$cxc-", "$crw-"], ["cxc ", "crw "], ["cxc\t", "crw\t"], ["cxc\u00a0", "crw\u00a0"], ["cxc  ", "crw  "], ["cxc\u3000", "crw\u3000"], ["CXC ", "CRW "], ["Cxc ", "Crw "], ["$CXC-", "$CRW-"], ["$codexclaw:CXC-", "$crw:CRW-"],
  ["cxc", "crw"], ["cxcx ", "crwx "], ["cxc/", "crw/"], ["$codexclaw:cxc", "$crw:crw"], ["$cxc ", "$crw "], ["$cxc", "$crw"], ["//", "//"], ["/ ", "/ "], ["$cxc-$cxc-", "$crw-$crw-"], ["/$cxc-", "/$crw-"], ["$cxc-/", "$crw-/"], ["cxc /", "crw /"], ["cxc cxc ", "crw crw "],
  ["\ufeff/", "\ufeff/"], [" $cxc-", " $crw-"], ["codexclaw:cxc-", "crw:crw-"], ["cxc-", "crw-"]];
const TOKENS = ["i", "p", "a", "b", "c", "d", "status", "reset", "I", "P", "A", "B", "C", "D", "STATUS", "Reset", "idle", "IDLE", "x", "proper", "constructor", "Constructor", "CONSTRUCTOR", "hasOwnProperty", "toString", "valueOf", "__proto__", "pp", "ab", "statuss", "p1", "1", "\u017ftatus", "K", "\u212a"];
const WORDS = ["orchestrate", "Orchestrate", "ORCHESTRATE", "oRcHeStRaTe", "orche\u017ftrate", "ORCHESTRATE\u017f", "orchestrat", "orchestrates", "orchestrate_"];
const WS = ["\t", "\u00a0", "\u3000", "\ufeff", "\u0085", "\u2028", "\u2029", "\r", "\u000b", "\u000c", "\u2003", "  ", "\u200b", "\u1680", "\u180e", "\u2000", "\u200a", "\u202f", "\u205f"];
const j = (o) => JSON.stringify(o), OK = j({ from: "P", to: "A", did: "challenged the plan" });
const J = [OK, j({ from: "C", to: "D", did: "ran tests", checkOutput: "233 pass", exitCode: 0 }), j({ from: "A", to: "B", did: "d", auditOutput: "o", auditVerdict: " NEAR-PASS ", auditResidual: "r", auditRounds: 2 }),
  j({ from: "P", to: "A", did: "x", planUnit: " u ", planPaths: [" a ", 1, "b"], workPhaseId: " wp1 ", override: true, testReceiptPath: " r.json " }), j({ from: "P", to: "A" }), j({ did: "x" }), "{}", j({ from: 1, to: "A" }), j({ from: "P", to: null }),
  j({ from: "P", to: "A", did: "a } b" }), j({ from: "P", to: "A", did: "a { b" }), j({ from: "P", to: "A", did: 'say "}" ok' }), j({ from: "P", to: "A", did: "c:\\" }), j({ from: "P", to: "A", did: "x", k: { a: [1, { b: 2 }] } }),
  '{"from":"P","to":"A","did":"x","from":"B"}', '{"from":"P","to":"A","did":"x","__proto__":{"a":1}}', '{"from":"C","to":"D","did":"x","checkOutput":"y","exitCode":1e999}', '{"from":"C","to":"D","did":"x","checkOutput":"y","exitCode":-0}',
  '{"from":"C","to":"D","did":"x","checkOutput":"y","exitCode":9007199254740993}', '{"from":"C","to":"D","did":"x","checkOutput":"y","exitCode":"0"}', '{"from":"P","to":"A","did":"\\ud83d\\ude00 \\u00e9"}', '{"from":"P","to":"A","did":"x",}', "{'from':'P'}", '{"from":"P" "to":"A"}', "{nope}",
  '{"from":"P","to":"A","did":"x"', OK + "}", '{"from":"P","to":"A","did":"tab\\there"}', '{"from":"P","to":"A","did":"raw\ttab"}', '{"a":01}', '{"a":[1,]}', '{"a":1.}', '{"a":nul}', '{"a":"\\x41"}', '{"a":"\\u12"}', '{"a":-}', '{ "from" : "P" , "to" : "A" , "did" : "spaced" }', OK + " "];
const FLAGSTR = ["--attest ", "--attest\t", "--attest\u00a0", "--attest  ", "--attest\u2028", "--attest\r", "--attest", "--attestx ", "--attest_ ", "--attest1 ", "--Attest ", "--ATTEST ", "-- attest ", "--attest: ", "--attest=", "--attest\u200b", "--attest\ufeff", "--attest-file ", "--attests "];
const TAILS = ["", " ", "  x", "\t", "{}", "}", "x", "\u00a0", "\u2028", "\u2028x", "\r", " --attest {}", ' {"a":1}'];
const cases = [], seen = new Set();
const answer = (p) => { const r = G.parseOrchestrateCommand(p); return r === null ? null : { verb: typeof r.verb === "function" ? "<function Object>" : r.verb, raw: r.rawAttest, attest: r.attest === null ? null : JSON.parse(JSON.stringify(r.attest)), err: r.attestError ?? null }; };
const add = (o, c = o) => { if (!seen.has(o + "\0" + c)) { seen.add(o + "\0" + c); cases.push({ o, c, want: answer(o) }); } };
const line = (p, body) => [p[0] + body, p[1] + body], none = ["", ""];
const lines = (parts, sep) => add(parts.map(([p, b]) => p[0] + b).join(sep), parts.map(([p, b]) => p[1] + b).join(sep));
for (const p of PRE) for (const tok of TOKENS) add(...line(p, "orchestrate " + tok));
for (const w of WORDS) for (const p of [none, PRE[1], PRE[3]]) add(...line(p, w + " p"));
for (const ws of WS) for (const body of ["orchestrate" + ws + "p", "orchestrate p" + ws, ws + "orchestrate p", "orchestrate p" + ws + "--attest " + OK, "orchestrate p --attest" + ws + OK, "orchestrate p --attest " + OK + ws, "orchestrate p" + ws + "x", "orchestrate ab" + ws + "x", "orchestrate p x" + ws + "y"]) add(...line(none, body));
for (const f of FLAGSTR) for (const p of [none, PRE[3]]) add(...line(p, "orchestrate A " + f + OK));
for (const js of J) { add(...line(none, "orchestrate A --attest " + js)); add(...line(PRE[4], "orchestrate d --attest " + js)); }
for (const t of TAILS) for (const js of [OK, "{}", J[2]]) add(...line(none, "orchestrate A --attest " + js + t));
for (const rest of ["x", "x --attest " + OK, "--attest", "--attest ", OK, "--attest " + OK + " --attest " + OK, "--attest " + OK + " " + OK, "--attest  \t " + OK + "  "]) for (const tok of ["p", "constructor", "x"]) add(...line(none, "orchestrate " + tok + " " + rest));
const [P1, P2, P3] = [[PRE[3], "orchestrate p"], [none, "orchestrate x"], [none, "orchestrate p x"]];
for (const sep of ["\n", "\r\n", "\r", "\n\n", "\n\u2028", "\u2028"]) { lines([[none, "context"], [PRE[2], "orchestrate b"], [none, "thanks"]], sep); lines([P2, P1], sep); lines([P3, [PRE[4], "orchestrate a"]], sep);
  lines([[none, "orchestrate p --attest " + OK + " tail"], [PRE[1], "orchestrate a"]], sep); lines([[none, "orchestrate p --attest {"], [none, "}"]], sep); lines([[none, ""], [none, "  "], P1], sep); lines([[none, "a"], P1, [none, "b"]], sep); lines([[none, "orchestrate"], [none, "p"]], sep); }
for (const p of ["", " ", "\n", "orchestrate", "orchestrate ", "orchestrate\u00a0", "please orchestrate p for me", "we should orchestrate proper tests", "orchestrate proper testing", "x orchestrate p", "orchestrate p.", "orchestrate p,", "orchestrate-p", "orchestrate/p", "\u0000orchestrate p", "orchestrate p\u0000"]) add(p);
const deep = 10001, deepRaw = '{"from":"P","to":"A","did":"x","k":' + "[".repeat(deep) + "]".repeat(deep) + "}";
writeFileSync(outDir + "/oracle-grammar.json", JSON.stringify({ cases, deep: { depth: deep, want: { ...answer("orchestrate A --attest " + deepRaw), raw: undefined } } }) + "\n");
