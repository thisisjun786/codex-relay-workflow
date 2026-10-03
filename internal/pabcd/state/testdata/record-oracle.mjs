// Records what the CXC v0.2.40 oracle's readStateStrict answers for the persisted state texts below; the Go tests
// replay oracle-restore.json (no Node at test time). Each case is the text of .codexclaw/sessions/rec-s1.json. The
// recorded answer is whether the oracle called it unreadable and the state it rebuilt, as an object (the Go test
// compares it compacted; the pretty layout is covered by the corpus goldens). A state that does not restore to itself when
// written and read again also records the second answer as "again". Date is frozen so a defaulted updatedAt is stable.
// Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> > oracle-restore.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";

const FROZEN = Date.parse("2026-01-01T00:00:00.000Z");
const RealDate = Date;
globalThis.Date = class extends RealDate {
  constructor(...a) { super(...(a.length ? a : [FROZEN])); }
  static now() { return FROZEN; }
};
const [oracleDist, workRoot] = process.argv.slice(2);
const { readStateStrict } = await import(oracleDist + "/state.js");

const id = (extra = {}) => ({ kind: "resolved", commitSha: "abc", dirty: false, capturedAt: "2026-01-01T00:00:00Z", ...extra });
const dims = (level) => Object.fromEntries(["goal", "constraint", "success", "ontology"].map((d) => [d, { level, known: ["k"], unknown: [], confidence: 1, EVIL: 1 }]));
const ready = { roundId: 2, dimensions: dims("max"), contradictions: [], assumptions: [{ id: "a", text: "x", recorded: true }], scanRounds: 1, lastScanRoundId: 1 };
const marker = (extra = {}) => ({ sessionId: "rec-s1", checkEpoch: "c1", closedWorkPhaseId: "wp1", nextWorkPhaseId: "wp2", ...extra });
const unv = (extra = {}) => ({ agentId: "a1", turnId: "t1", agentType: "executor", attempts: 3, receiptClaimed: "r", recordedAt: "2026-01-01T00:00:00Z", resolvable: true, ...extra });
const without = (o, k) => { const c = { ...o }; delete c[k]; return c; };
const cases = {};
const add = (name, value) => { cases[name] = typeof value === "string" ? value : JSON.stringify(value); };

for (const [name, text] of Object.entries({
  not_json: "{not json", empty_file: "", whitespace_only: " \n", trailing_garbage: '{"phase":"P"} x', bom_prefix: '\uFEFF{"phase":"P"}',
  array: "[]", null: "null", number: "5", string: '"IDLE"', boolean: "true", duplicate_phase_key: '{"phase":"P","phase":"B"}',
  infinite_count: '{"phase":"P","stopBlockCount":1e999,"stopMetricCursor":1e999,"idleEditNudges":1e999}',
  count_text_forms: '{"phase":"P","stopBlockCount":2.9E0,"stopMetricCursor":-0,"stopBlockTotal":9007199254740991}',
  escaped_slug: '{"phase":"P","slug":"\\u00e9\\n\\"q\\" \\\\ \\b\\f\\u0001\\u001f\\u007f\\u0000"}',
})) add(name, text);
add("no_phase", {}); add("phase_number", { phase: 5 }); add("phase_lowercase", { phase: "idle" }); add("phase_unknown", { phase: "Z" });
for (const p of ["IDLE", "I", "P", "A", "B", "C", "D"]) add("phase_" + p, { phase: p });
add("slug_text", { phase: "P", slug: "my-slug" }); add("slug_number", { phase: "P", slug: 5 });
add("slug_escape_text", '{"phase":"P","slug":"\\\\u2028 \\u2028"}');
add("slug_markup", { phase: "P", slug: "<script>a&b</script>" }); add("slug_separators", { phase: "P", slug: "a\u2028b\u2029c" }); add("slug_astral", { phase: "P", slug: "x\u{1F600}y" });
add("updatedAt_text", { phase: "P", updatedAt: "2025-05-05T05:05:05.000Z" }); add("updatedAt_number", { phase: "P", updatedAt: 5 });
add("flags_set", { phase: "P", flags: { auditPassed: true, checkPassed: true, interview: true, bogus: 1 } });
for (const [n, v] of Object.entries({ flags_null: null, flags_array: [1], flags_text: "x", flags_loose: { auditPassed: 1, checkPassed: "true" } })) add(n, { phase: "P", flags: v });
add("interview_forged_flag", { phase: "I", flags: { interview: true }, interview: { roundId: 1, dimensions: {}, contradictions: [], assumptions: [] } });
add("interview_ready", { phase: "P", flags: { interview: false }, interview: ready });
add("interview_ready_with_extras", { phase: "P", interview: { ...ready, EVIL: 1, ontologySchema: [{ name: "E", fields: ["f"], relationships: [{ to: "F", kind: "has" }] }] } });
for (const [n, v] of Object.entries({ interview_text: "x", interview_array: [], interview_null: null, interview_empty: {} })) add(n, { phase: "P", interview: v });
for (const [n, v] of Object.entries({ superseded_empty: "", superseded_text: "next", superseded_number: 5, superseded_null: null })) add(n, { phase: "P", supersededBy: v });
for (const [n, v] of Object.entries({ turns_valid: ["a", "b"], turns_mixed: [1, "a"], turns_text: "t", turns_empty: [], turns_null: null, turns_many: Array.from({ length: 120 }, (_, i) => "t" + i) })) add(n, { phase: "P", injectedTurns: v });
for (const [n, v] of Object.entries({ last_valid: "B", last_idle: "IDLE", last_unknown: "Z", last_number: 5 })) add(n, { phase: "P", lastInjectedPhase: v });
add("active_at_b", { phase: "B", orchestrationActive: true }); add("active_at_idle", { phase: "IDLE", orchestrationActive: true }); add("active_text", { phase: "B", orchestrationActive: "yes" });
for (const v of ["IDLE", "B", "Z", 5]) add("stop_phase_" + v, { phase: "P", stopBlockPhase: v });
add("counts_fractional", { phase: "P", stopBlockCount: 3.7, stopMetricCursor: 12.9, stopBlockTotal: 0.5, idleEditNudges: 2.5 });
add("counts_negative", { phase: "P", stopBlockCount: -1, stopMetricCursor: -0.5, stopBlockTotal: -3, idleEditNudges: -3 });
add("counts_text", { phase: "P", stopBlockCount: "5", stopMetricCursor: null, stopBlockTotal: true, idleEditNudges: [1] });
add("counts_big", '{"phase":"P","stopBlockCount":1000000000000000100,"stopMetricCursor":1e21,"stopBlockTotal":1.5e300,"idleEditNudges":123456789012345680000}');
add("counts_large", { phase: "P", stopBlockCount: 9007199254740993, stopMetricCursor: 4294967296, stopBlockTotal: 41, idleEditNudges: 7 });
for (const [n, v] of Object.entries({ work_empty: "", work_text: "wp1", work_number: 5 })) add(n, { phase: "P", stopBlockWorkPhaseId: v, stopBlockTurnId: v, memoryWriteTurn: v });
add("notice_set", { phase: "P", stopBlockCapNotified: true, loopArmSeen: true, memoryWriteRequested: true, memoryWriteGrant: true });
add("notice_loose", { phase: "P", stopBlockCapNotified: "true", loopArmSeen: "yes", memoryWriteRequested: 1, memoryWriteGrant: [] });
add("unverified_valid", { phase: "P", unverifiedSubagents: [unv()] });
add("unverified_defaults", { phase: "P", unverifiedSubagents: [{ agentId: "a", recordedAt: "r", attempts: 1.5, agentType: 3, turnId: 4, receiptClaimed: 5, resolvable: "false" }] });
add("unverified_unresolvable", { phase: "P", unverifiedSubagents: [unv({ resolvable: false, attempts: -2 })] });
add("unverified_claim_cut", { phase: "P", unverifiedSubagents: [unv({ receiptClaimed: "é".repeat(300) })] });
add("unverified_bad_items", { phase: "P", unverifiedSubagents: [unv(), null, [], 0, "x", without(unv(), "agentId"), without(unv(), "recordedAt"), unv({ agentId: "a2" })] });
for (const [n, v] of Object.entries({ unverified_object: {}, unverified_text: "x", unverified_null: null, unverified_empty: [] })) add(n, { phase: "P", unverifiedSubagents: v });
add("unverified_cap_64", { phase: "P", unverifiedSubagents: Array.from({ length: 64 }, (_, i) => unv({ agentId: "a" + i })) });
add("unverified_cap_exceeded", { phase: "P", unverifiedSubagents: Array.from({ length: 66 }, (_, i) => unv({ agentId: "a" + i })) });
add("unverified_claim_cut_astral", { phase: "P", unverifiedSubagents: [unv({ receiptClaimed: "a".repeat(254) + "\u{1F600}" + "b" })] });
add("unverified_corrupt_sticky", { phase: "P", unverifiedCorrupt: true, unverifiedSubagents: [unv()] });
add("unverified_corrupt_text", { phase: "P", unverifiedCorrupt: "true" });
for (const [n, v] of Object.entries({
  entry_clean: id(), entry_dirty: id({ dirty: true, treeHash: "h" }), entry_dirty_no_hash: id({ dirty: true }), entry_dirty_text: id({ dirty: "yes" }),
  entry_hash_number: id({ treeHash: 5 }), entry_hash_null: id({ treeHash: null }), entry_unavailable: id({ kind: "unavailable", commitSha: "" }),
  entry_kind_other: id({ kind: "other" }), entry_no_time: without(id(), "capturedAt"), entry_no_commit: without(id(), "commitSha"),
  entry_root_abs: id({ sourceRoot: "/ws/src" }), entry_root_rel: id({ sourceRoot: "src" }), entry_root_null: id({ sourceRoot: null }), entry_root_number: id({ sourceRoot: 5 }),
  entry_extras: id({ dirty: true, treeHash: "h", sourceRoot: "/ws", extra: 1 }), entry_text: "chat", entry_array: [], entry_null: null,
})) add(n, { phase: "B", phaseEntrySource: v });
add("entry_hash_empty", { phase: "B", phaseEntrySource: id({ treeHash: "" }) });
add("entry_dirty_empty_hash", { phase: "B", phaseEntrySource: id({ dirty: true, treeHash: "" }) });
for (const p of ["P", "IDLE", "C"]) add("entry_outside_b_" + p, { phase: p, phaseEntrySource: id() });
for (const [n, v] of Object.entries({ bound_abs: "/ws/src", bound_rel: "src", bound_empty: "", bound_number: 5 })) add(n, { phase: "B", boundSourceRoot: v });
add("bound_at_idle", { phase: "IDLE", boundSourceRoot: "/ws" });
for (const p of ["A", "B", "IDLE"]) add("plan_at_" + p, { phase: p, planUnit: "devlog/_plan/260101_x", planEpoch: "e1", checkEpoch: "c1" });
add("plan_empty", { phase: "A", planUnit: "", planEpoch: "" }); add("plan_number", { phase: "A", planUnit: 1, planEpoch: 2, checkEpoch: 3 });
for (const p of ["C", "B"]) add("check_at_" + p, { phase: p, checkEpoch: "c1" }); add("check_empty", { phase: "C", checkEpoch: "" });
add("marker_valid_idle", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker() });
add("marker_valid_c", { phase: "C", checkEpoch: "c1", dcloseRecovery: marker() });
add("marker_valid_b", { phase: "B", checkEpoch: "c1", dcloseRecovery: marker() });
add("marker_null_next", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker({ nextWorkPhaseId: null }) });
add("marker_absent_next", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: without(marker(), "nextWorkPhaseId") });
for (const [n, v] of Object.entries({ marker_number_next: 7, marker_empty_next: "", marker_array_next: [], marker_object_next: {} })) add(n, { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker({ nextWorkPhaseId: v }) });
add("marker_foreign", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker({ sessionId: "other" }) });
add("marker_no_epoch", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker({ checkEpoch: "" }) });
add("marker_epoch_number", { phase: "IDLE", dcloseRecovery: marker({ checkEpoch: 1 }) });
add("marker_no_phase_id", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker({ closedWorkPhaseId: "" }) });
add("marker_legacy_input", { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: marker({ legacy: true, extra: 1 }) });
for (const [n, v] of Object.entries({ marker_array: [], marker_text: "x", marker_null: null })) add(n, { phase: "IDLE", checkEpoch: "c1", dcloseRecovery: v });
add("unknown_keys", { phase: "P", evil: "x", nested: { a: 1 }, flags: { bogus: 1 } });
add("keys_reversed", '{"dcloseRecovery":null,"checkEpoch":null,"planEpoch":null,"planUnit":null,"phase":"P"}');
add("full_state", { phase: "B", sessionId: "other-id", slug: "s", updatedAt: "2026-02-03T04:05:06.789Z", flags: { interview: true, auditPassed: true, checkPassed: false }, supersededBy: "x", injectedTurns: ["t1"], lastInjectedPhase: "B", orchestrationActive: true, interview: ready, stopBlockPhase: "B", stopBlockCount: 2, stopBlockWorkPhaseId: "wp1", stopMetricCursor: 3, stopBlockTotal: 4, stopBlockTurnId: "t2", stopBlockCapNotified: true, loopArmSeen: true, idleEditNudges: 5, memoryWriteRequested: true, memoryWriteTurn: "t3", memoryWriteGrant: true, unverifiedSubagents: [unv()], unverifiedCorrupt: true, phaseEntrySource: id({ dirty: true, treeHash: "h", sourceRoot: "/ws" }), boundSourceRoot: "/ws", planUnit: "u", planEpoch: "e", checkEpoch: "c", dcloseRecovery: marker() });

const out = [];
for (const [name, raw] of Object.entries(cases)) {
  const cwd = mkdtempSync(join(workRoot, "s-"));
  mkdirSync(join(cwd, ".codexclaw", "sessions"), { recursive: true });
  writeFileSync(join(cwd, ".codexclaw", "sessions", "rec-s1.json"), raw);
  const { state, unreadable } = readStateStrict(cwd, "rec-s1");
  writeFileSync(join(cwd, ".codexclaw", "sessions", "rec-s1.json"), JSON.stringify(state, null, 2));
  const again = readStateStrict(cwd, "rec-s1");
  const entry = { id: name, raw, unreadable, state };
  if (again.unreadable || JSON.stringify(again.state) !== JSON.stringify(state)) entry.again = again.state; // a state that does not restore to itself
  out.push(entry);
  rmSync(cwd, { recursive: true, force: true });
}
process.stdout.write("[\n" + out.map((o) => JSON.stringify(o)).join(",\n") + "\n]\n");
