// Synthetic direct answers from CXC v0.2.40 (commit 3c1459ac), Node v24.
// Usage: node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist
// Private helpers are extracted unchanged from that same compiled oracle.
// No writes to the oracle. Go replay needs no Node. Not representable: lone
// surrogate literals and a pending task with explicitly present outcome: "".
import { readFileSync } from "node:fs";
const [dist] = process.argv.slice(2);
const g = await import(dist + "/goalplan.js");
const code = readFileSync(new URL(dist + "/goalplan.js"), "utf8");
const privateFunction = (name) => {
  const start = code.indexOf("function " + name + "(");
  if (start < 0) throw new Error("missing oracle helper " + name);
  const end = code.indexOf("\n}\n", start);
  return code.slice(start, end + 3);
};
const superseded = new Function(privateFunction("supersededIntegrityReasons") + "; return supersededIntegrityReasons;")();
const phase = (id, more = {}) => ({id, title: id, status: "pending", tasks: [], criteriaIds: [], ...more});
const task = (id, more = {}) => ({id, title: id, status: "pending", ...more});
const plan = (more = {}) => ({objective: "synthetic integrity", slug: "integrity", createdAt: "2026-01-01T00:00:00.000Z", updatedAt: "2026-01-01T00:00:00.000Z", activeWorkPhaseId: null, host: {armed: false, armedAt: null, source: "none"}, schemaVersion: 3, workPhases: [], criteria: [], ...more});
const cases = [];
const add = (id, p, marker = false) => cases.push({id, plan: p, marker, definition: g.goalplanDefinitionIntegrityReasons(p), completion: g.goalplanDependencyCompletionReasons(p), superseded: superseded(p), version: g.effectiveSchemaVersion(p, marker), qa: g.computeQaRequired(p)});
add("empty", plan());
add("duplicates_utf16", plan({workPhases: ["\uE000", "\u{10000}", "\uE000", "\u{10000}"].map(id => phase(id))}));
add("cycle_roots_utf16", plan({workPhases: [phase("\uE000", {dependsOn: ["z"]}), phase("z", {dependsOn: ["\uE000"]}), phase("\u{10000}", {dependsOn: ["x"]}), phase("x", {dependsOn: ["\u{10000}"]})]}));
add("cycle_edges_utf16", plan({workPhases: [phase("a", {dependsOn: ["\uE000", "\u{10000}"]}), phase("\uE000", {dependsOn: ["a"]}), phase("\u{10000}", {dependsOn: ["a"]})]}));
add("last_phase_wins", plan({workPhases: [phase("base"), phase("base", {status: "done"}), phase("leaf", {status: "done", dependsOn: ["ghost", "base", "ghost"]})]}));
add("last_task_wins", plan({workPhases: [phase("p", {tasks: [task("base"), task("base", {status: "done", outcome: "proof"}), task("leaf", {status: "done", outcome: "proof", dependsOn: ["base"]})]})]}));
add("last_decision_wins", plan({decisions: [{id: "d", status: "open"}, {id: "d", status: "decided"}], workPhases: [phase("p", {status: "done", awaitsDecision: ["d"]})]}));
add("decision_order", plan({decisions: [{id: "open", status: "open"}], workPhases: [phase("p", {status: "done", awaitsDecision: ["z", "z", "open", "a", "a"]})]}));
add("cross_phase_reason_order", plan({workPhases: [phase("first", {dependsOn: ["missing"]}), phase("second", {awaitsDecision: ["unknown"]})]}));
add("task_cycle_before_outcome", plan({workPhases: [phase("p", {tasks: [task("a", {status: "done", dependsOn: ["b"]}), task("b", {dependsOn: ["a"]})]})]}));
add("criteria_repeats_kept", plan({workPhases: [phase("p", {criteriaIds: ["ghost", "ghost", "ghost", "ghost", "other"]})]}));
add("self_and_missing", plan({workPhases: [phase("p", {dependsOn: ["p", "gone", "gone"], tasks: [task("t", {dependsOn: ["t", "missing", "missing"]})]})]}));
add("blank_outcomes", plan({workPhases: [phase("p", {tasks: [task("done", {status: "done", outcome: "\uFEFF"}), task("pending", {outcome: " "}), task("nel", {status: "done", outcome: "\u0085"})]})]}));
add("superseded_reasons", plan({workPhases: [phase("missing", {status: "superseded"}), phase("blank", {status: "superseded", supersededBy: "\uFEFF"}), phase("self", {status: "superseded", supersededBy: "self"}), phase("unknown", {status: "superseded", supersededBy: "ghost"}), phase("chain", {status: "superseded", supersededBy: "self"})]}));
add("superseded_first_target", plan({workPhases: [phase("old", {status: "superseded", supersededBy: "target"}), phase("target", {status: "done"}), phase("target", {status: "superseded", supersededBy: "old"})]}));
add("absent_schema", plan({schemaVersion: undefined}));
add("marker_promotes", plan({schemaVersion: undefined}), true);
add("marker_keeps_three", plan(), true);
add("fractional_schema", plan({schemaVersion: 2.5}), true);
for (const surface of [undefined, "logic", "web", "tui", "desktop", "other"]) add("qa_" + (surface ?? "absent"), plan({criteria: [{id: "c", surface}]}));
const marker = g.schemaMarkerPath("/synthetic", "integrity");
process.stdout.write(JSON.stringify({oracle: "CXC v0.2.40 3c1459ac", cases, marker}, null, 2) + "\n");
