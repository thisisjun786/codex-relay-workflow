// Records what the CXC v0.2.40 oracle's revival functions for review rounds, plan files, source identities and the
// final gate answer (pabcd-state/src/goalplan.ts:333-467); the Go tests replay oracle-revive.json (no Node at test
// time). The functions are private to the module, so each case puts its raw value in a goalplan.json and reads the plan
// back with readGoalplan, the way every caller reaches them; the recorded answer is the compact JSON of the plan's
// reviewRounds and finalGate as the oracle revived them (an absent field is absent), or null when readGoalplan
// refused the plan. The file holds each case's input beside that answer. Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <short work dir> > oracle-revive.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";

const [oracleDist, workRoot] = process.argv.slice(2);
const { readGoalplan } = await import(oracleDist + "/goalplan.js");

const T = "2026-01-01T00:00:00.000Z";
const lane = (extra = {}) => ({ launchId: "r1-20260101000000", ...extra });
const round = (extra = {}) => ({ roundId: "r1", purpose: "plan_audit", planPath: "devlog/_plan/260101_demo", planSha256: "ab12", status: "pending", lane: lane(), openedAt: T, ...extra });
const ident = (extra = {}) => ({ kind: "resolved", commitSha: "abc123", dirty: false, capturedAt: T, ...extra });
const gate = (extra = {}) => ({ status: "pending", qaRequired: false, updatedAt: T, ...extra });
const without = (o, ...keys) => { const c = { ...o }; for (const k of keys) delete c[k]; return c; };
const good = round({ roundId: "ok" });
const cases = {};
const rounds = (name, value) => { cases[name] = { reviewRounds: value }; };
const finalGate = (name, value) => { cases[name] = { finalGate: value }; };

// reviewRounds: what is not a list, a list that holds nothing, accepted rounds.
cases.rounds_absent = {};
for (const [n, v] of Object.entries({ null: null, object: {}, string: "r1", number: 5, bool: true })) rounds("rounds_" + n, v);
rounds("rounds_empty", []);
rounds("round_minimal", [round()]);
rounds("round_full", [round({ status: "approved", closedAt: T, ownerSessionId: "rec-s1", workPhaseId: "wp1", planUnit: "devlog/_plan/260101_demo", planEpoch: "e1",
  planFiles: [{ path: "a/000_plan.md", sha256: "aa" }, { path: "a/010_plan.md", sha256: "bb" }],
  lane: lane({ reviewerSession: "rev-1", workspaceRoot: "/ws", artifactSha256: "cc", verdict: "pass", sourceIdentity: ident({ dirty: true, treeHash: "dd", sourceRoot: "/ws" }) }) })]);
rounds("round_purposes", [round({ roundId: "r1", purpose: "plan_audit" }), round({ roundId: "r2", purpose: "final_gate" })]);
rounds("round_statuses_valid", ["pending", "launching", "in_flight", "approved", "changes_requested", "inconclusive"].map((status, i) => round({ roundId: "r" + (i + 1), status })));
rounds("round_order_and_duplicates_kept", [round({ roundId: "r2" }), round({ roundId: "r1" }), round({ roundId: "r2", planPath: "other" })]);
rounds("round_strings_pass_through", [round({ roundId: "r-계획", planPath: "계획/<000>&.md", planSha256: "" })]);
rounds("round_extra_keys_dropped", [round({ evil: 1, nested: { a: [1] }, lane: lane({ evil: 2 }) })]);

// reviewRounds: an entry that is not a round is dropped, the others stay.
const bad = (name, entry) => rounds("drop_" + name, [entry, good]);
for (const [n, v] of Object.entries({ entry_null: null, entry_number: 5, entry_string: "x", entry_array: [], entry_empty_object: {},
  roundId_missing: without(round(), "roundId"), roundId_empty: round({ roundId: "" }), roundId_number: round({ roundId: 1 }), roundId_null: round({ roundId: null }),
  purpose_missing: without(round(), "purpose"), purpose_other: round({ purpose: "audit" }), purpose_case: round({ purpose: "Plan_Audit" }), purpose_number: round({ purpose: 5 }),
  planPath_missing: without(round(), "planPath"), planPath_number: round({ planPath: 5 }), planPath_null: round({ planPath: null }),
  planSha256_missing: without(round(), "planSha256"), planSha256_number: round({ planSha256: 5 }),
  status_missing: without(round(), "status"), status_done: round({ status: "done" }), status_case: round({ status: "PENDING" }), status_number: round({ status: 5 }), status_null: round({ status: null }),
  lane_missing: without(round(), "lane"), lane_null: round({ lane: null }), lane_string: round({ lane: "r1" }), lane_array: round({ lane: [] }), lane_empty: round({ lane: {} }),
  lane_launchId_empty: round({ lane: lane({ launchId: "" }) }), lane_launchId_number: round({ lane: lane({ launchId: 5 }) }) })) bad(n, v);
rounds("keep_planPath_and_planSha256_empty", [round({ planPath: "", planSha256: "" })]);

// openedAt, closedAt and the binding fields.
for (const [n, v] of Object.entries({ missing: without(round(), "openedAt"), number: round({ openedAt: 5 }), null: round({ openedAt: null }), empty: round({ openedAt: "" }) })) rounds("openedAt_" + n, [v]);
for (const [n, v] of Object.entries({ empty: "", number: 5, null: null, set: T })) rounds("closedAt_" + n, [round({ closedAt: v })]);
for (const [n, v] of Object.entries({ empty: "", number: 5, null: null, set: "x" })) rounds("binding_" + n, [round({ ownerSessionId: v, workPhaseId: v, planUnit: v, planEpoch: v })]);
rounds("binding_mixed", [round({ ownerSessionId: "s", workPhaseId: "", planUnit: 5, planEpoch: "e" })]);

// The lane.
const l = (name, extra) => rounds("lane_" + name, [round({ lane: lane(extra) })]);
l("strings", { reviewerSession: "rev", workspaceRoot: "/w", artifactSha256: "aa" });
l("empty_strings_kept", { reviewerSession: "", workspaceRoot: "", artifactSha256: "" });
l("non_strings_dropped", { reviewerSession: 5, workspaceRoot: null, artifactSha256: {} });
for (const v of ["pass", "near-pass", "fail", "PASS", "ok", "", 5, null]) l("verdict_" + String(v), { verdict: v });
l("verdict_and_identity_order", { sourceIdentity: ident(), verdict: "fail", reviewerSession: "rev" });
l("identity_invalid_dropped", { sourceIdentity: { kind: "other" }, verdict: "pass" });

// planFiles: the whole list or nothing.
const pf = (name, value) => rounds("files_" + name, [round({ planFiles: value })]);
pf("valid", [{ path: "a/000.md", sha256: "aa" }, { path: "a/010.md", sha256: "bb" }]);
pf("empty", []); pf("null", null); pf("object", {}); pf("string", "x");
pf("extra_keys_dropped", [{ path: "a", sha256: "b", size: 3 }]);
pf("path_absolute_and_dotdot_accepted", [{ path: "/etc/hostname", sha256: "x" }, { path: "../../up", sha256: "y" }]);
pf("sha256_not_hex_accepted", [{ path: "a", sha256: "not hex" }]);
for (const [n, v] of Object.entries({ entry_null: null, entry_number: 5, entry_string: "x", entry_array: [], path_missing: { sha256: "b" }, path_empty: { path: "", sha256: "b" },
  path_number: { path: 5, sha256: "b" }, sha256_missing: { path: "a" }, sha256_empty: { path: "a", sha256: "" }, sha256_number: { path: "a", sha256: 5 } })) pf("one_bad_" + n, [{ path: "keep", sha256: "k" }, v]);

// reviveSourceIdentity, reached through the final gate (and, above, through a lane).
const si = (name, value) => finalGate("identity_" + name, gate({ sourceIdentity: value }));
si("resolved", ident()); si("unavailable", ident({ kind: "unavailable", commitSha: "" }));
si("dirty_with_tree", ident({ dirty: true, treeHash: "dd" })); si("dirty_without_tree_accepted", ident({ dirty: true }));
si("tree_empty_kept", ident({ treeHash: "" })); si("tree_number_dropped", ident({ treeHash: 5 })); si("tree_null_dropped", ident({ treeHash: null }));
si("dirty_tree_number_keeps_dirty", ident({ dirty: true, treeHash: 5 }));
si("root_absolute", ident({ sourceRoot: "/ws/a" })); si("root_dotdot_accepted", ident({ sourceRoot: "/a/../b" }));
si("extra_keys_dropped", ident({ evil: 1 }));
si("commitSha_missing_defaults", without(ident(), "commitSha")); si("commitSha_number_defaults", ident({ commitSha: 5 }));
si("dirty_missing_defaults", without(ident(), "dirty")); si("dirty_string_reads_clean", ident({ dirty: "yes" })); si("dirty_one_reads_clean", ident({ dirty: 1 }));
si("capturedAt_missing_defaults", without(ident(), "capturedAt")); si("capturedAt_number_defaults", ident({ capturedAt: 5 })); si("capturedAt_empty_kept", ident({ capturedAt: "" }));
si("bare_kind_only", { kind: "resolved" });
for (const [n, v] of Object.entries({ kind_missing: without(ident(), "kind"), kind_case: ident({ kind: "Resolved" }), kind_other: ident({ kind: "unknown" }), kind_number: ident({ kind: 1 }),
  root_relative: ident({ sourceRoot: "ws" }), root_empty: ident({ sourceRoot: "" }), root_null: ident({ sourceRoot: null }), root_number: ident({ sourceRoot: 5 }),
  null: null, string: "x", array: [], number: 5, bool: true })) si("drop_" + n, v);

// reviveFinalGate.
for (const [n, v] of Object.entries({ null: null, string: "x", array: [], number: 5, bool: true, empty: {} })) finalGate("gate_" + n, v);
finalGate("gate_minimal", gate());
for (const status of ["pending", "in_flight", "approved", "inconclusive"]) finalGate("gate_status_" + status, gate({ status }));
for (const [n, v] of Object.entries({ missing: without(gate(), "status"), other: gate({ status: "done" }), round_status: gate({ status: "changes_requested" }), launching: gate({ status: "launching" }),
  case: gate({ status: "Approved" }), number: gate({ status: 5 }), null: gate({ status: null }),
  qaRequired_missing: without(gate(), "qaRequired"), qaRequired_string: gate({ qaRequired: "true" }), qaRequired_one: gate({ qaRequired: 1 }), qaRequired_null: gate({ qaRequired: null }) })) finalGate("gate_drop_" + n, v);
finalGate("gate_qa_required", gate({ qaRequired: true }));
for (const [n, v] of Object.entries({ missing: without(gate(), "updatedAt"), number: gate({ updatedAt: 5 }), null: gate({ updatedAt: null }), empty: gate({ updatedAt: "" }) })) finalGate("gate_updatedAt_" + n, v);
for (const [n, v] of Object.entries({ strings: { reviewRoundId: "r2", testReceiptPath: "t.json", qaReceiptPath: "q.json" }, empty_strings_kept: { reviewRoundId: "", testReceiptPath: "", qaReceiptPath: "" },
  non_strings_dropped: { reviewRoundId: 5, testReceiptPath: null, qaReceiptPath: {} } })) finalGate("gate_" + n, gate(v));
for (const v of ["pass", "near-pass", "fail", "PASS", 5, null]) finalGate("gate_verdict_" + String(v), gate({ verdict: v }));
finalGate("gate_full", gate({ status: "approved", qaRequired: true, reviewRoundId: "r3", testReceiptPath: "t", qaReceiptPath: "q", verdict: "near-pass", sourceIdentity: ident({ treeHash: "h" }) }));
finalGate("gate_key_order_follows_revive", { sourceIdentity: ident(), verdict: "pass", qaReceiptPath: "q", testReceiptPath: "t", reviewRoundId: "r", updatedAt: T, qaRequired: true, status: "approved" });
finalGate("gate_extra_keys_dropped", gate({ evil: 1 }));
cases.gate_and_rounds_are_independent = { reviewRounds: [round(), { roundId: "" }], finalGate: gate({ status: "approved" }) };

const out = {};
for (const [name, input] of Object.entries(cases)) {
  const root = mkdtempSync(join(workRoot, "g-"));
  const dir = join(root, ".codexclaw", "goalplans", "rec-plan");
  mkdirSync(dir, { recursive: true });
  const plan = { objective: "o", slug: "rec-plan", workPhases: [], criteria: [], host: { armed: false, armedAt: null, source: "none" }, ...input };
  writeFileSync(join(dir, "goalplan.json"), JSON.stringify(plan));
  const read = readGoalplan(root, "rec-plan");
  out[name] = { input, oracle: read === null ? null : JSON.stringify({ reviewRounds: read.reviewRounds, finalGate: read.finalGate }) };
  rmSync(root, { recursive: true, force: true });
}
const names = Object.keys(out);
process.stdout.write("{\n" + names.map((n, i) => JSON.stringify(n) + ": " + JSON.stringify(out[n]) + (i < names.length - 1 ? "," : "")).join("\n") + "\n}\n"); // one case per line
