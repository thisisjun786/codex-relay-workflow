// Records what CXC v0.2.40's identityHash answers over the cases below; the Go test replays
// identity-oracle.json (no Node at test time). Recorded with Node v24.20.0 as
//   node record-identity.mjs <oracle cxc-ops dist dir> <repo root> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d). A case's handler is stored as the raw JSON
// document text the oracle parsed, so a spelling JSON.stringify cannot carry (the timeout
// 1e400 literal, which JSON.parse reads as Infinity) survives in the fixture instead of
// being re-encoded to null.
import { readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";

const [dist, root, out] = process.argv.slice(2);
if (!dist || !root || !out) throw new Error("usage: node record-identity.mjs <oracle dist dir> <repo root> <out file>");
const { identityHash } = await import(resolve(dist, "hook-trust.js"));
// plugins/codexclaw/components/cxc-ops/dist -> plugins/codexclaw, where the hook documents sit.
const oracleHooks = resolve(dist, "..", "..", "..", "hooks");
const document = (name) => JSON.parse(readFileSync(join(oracleHooks, name), "utf8"));
const group = (hooks, event, matcher) => {
  const groups = hooks[event];
  const found = matcher === undefined ? groups[0] : groups.find((candidate) => candidate.matcher === matcher);
  if (!found) throw new Error(`no ${event} group with matcher ${String(matcher)}`);
  return found;
};
const crwStop = JSON.parse(readFileSync(join(root, "plugins", "crw", "wiring", "hooks", "stop-recording-completion.json"), "utf8"));
const crwStopHandler = group(crwStop.hooks, "Stop").hooks[0];
const subagentStopHandler = group(document("subagent-stop-verifying-evidence.json").hooks, "SubagentStop", "^(executor|worker)$").hooks[0];
const permissionRequestHandler = group(document("permission-request-allowing-agent-thread.json").hooks, "PermissionRequest", "*").hooks[0];
const sessionStartAdvisoryHandler = group(document("session-start-advising-agent-thread-permissions.json").hooks, "SessionStart").hooks[0];

const handler = (extra = {}) => JSON.stringify({ type: "command", command: "echo ok", ...extra });
const labels = { PreToolUse: "pre_tool_use", PostToolUse: "post_tool_use", SessionStart: "session_start", UserPromptSubmit: "user_prompt_submit", Stop: "stop", SubagentStart: "subagent_start", SubagentStop: "subagent_stop", PreCompact: "pre_compact", PostCompact: "post_compact", PermissionRequest: "permission_request" };

const cases = [
  ...Object.keys(labels).map((event) => ({ name: `label_${labels[event]}`, event, handler: handler() })),
  { name: "stop_matcher_dropped", event: "Stop", matcher: "^ignored$", handler: handler() },
  { name: "user_prompt_submit_matcher_dropped", event: "UserPromptSubmit", matcher: "^ask$", handler: handler() },
  { name: "pre_tool_use_matcher_kept", event: "PreToolUse", matcher: "^kept$", handler: handler() },
  { name: "pre_tool_use_empty_matcher", event: "PreToolUse", matcher: "", handler: handler() },
  { name: "oracle_subagent_stop_group", event: "SubagentStop", matcher: "^(executor|worker)$", handler: JSON.stringify(subagentStopHandler) },
  { name: "oracle_permission_request_group", event: "PermissionRequest", matcher: "*", handler: JSON.stringify(permissionRequestHandler) },
  { name: "oracle_session_start_advisory_group", event: "SessionStart", handler: JSON.stringify(sessionStartAdvisoryHandler) },
  { name: "crw_stop_live", document: "plugins/crw/wiring/hooks/stop-recording-completion.json", event: "Stop", handler: JSON.stringify(crwStopHandler) },
  { name: "proto_constructor", event: "constructor", handler: handler() },
  { name: "proto_toString", event: "toString", handler: handler() },
  { name: "proto_valueOf", event: "valueOf", handler: handler() },
  { name: "proto_proto", event: "__proto__", handler: handler() },
  { name: "proto_constructor_matcher", event: "constructor", matcher: "^x$", handler: handler() },
  { name: "proto_proto_matcher", event: "__proto__", matcher: "^x$", handler: handler() },
  { name: "event_foreign", event: "then", handler: handler() },
  { name: "event_foreign_precedes_type", event: "then", handler: handler({ type: 5 }) },
  { name: "command_empty", event: "Stop", handler: handler({ command: "" }) },
  { name: "command_missing", event: "Stop", handler: '{"type":"command"}' },
  { name: "command_null", event: "Stop", handler: JSON.stringify({ type: "command", command: null }) },
  { name: "command_number", event: "Stop", handler: JSON.stringify({ type: "command", command: 5 }) },
  { name: "command_nonascii", event: "Stop", handler: handler({ command: "echo \u00e9\u65e5\u{1f600}" }) },
  { name: "command_u2028", event: "Stop", handler: handler({ command: "echo \u2028x" }) },
  { name: "command_lone_surrogate", event: "Stop", handler: handler({ command: "echo \ud800" }) },
  { name: "type_shell", event: "Stop", handler: handler({ type: "shell" }) },
  { name: "type_null", event: "Stop", handler: JSON.stringify({ type: null, command: "echo ok" }) },
  { name: "type_number", event: "Stop", handler: JSON.stringify({ type: 5, command: "echo ok" }) },
  { name: "type_boolean", event: "Stop", handler: JSON.stringify({ type: true, command: "echo ok" }) },
  { name: "type_exponent", event: "Stop", handler: JSON.stringify({ type: 1e-7, command: "echo ok" }) },
  { name: "type_object", event: "Stop", handler: JSON.stringify({ type: {}, command: "echo ok" }) },
  { name: "type_1e400", event: "Stop", handler: '{"type":1e400,"command":"echo ok"}' },
  { name: "type_negative_1e400", event: "Stop", handler: '{"type":-1e400,"command":"echo ok"}' },
  { name: "type_object_valueOf", event: "Stop", handler: JSON.stringify({ type: { valueOf: 5 }, command: "echo ok" }) },
  { name: "type_array", event: "Stop", handler: JSON.stringify({ type: [1, null, "a"], command: "echo ok" }) },
  { name: "type_empty_string", event: "Stop", handler: JSON.stringify({ type: "", command: "echo ok" }) },
  { name: "type_toString_null", event: "Stop", handler: JSON.stringify({ type: { toString: null }, command: "echo ok" }) },
  { name: "type_toString_number", event: "Stop", handler: JSON.stringify({ type: { toString: 5 }, command: "echo ok" }) },
  { name: "type_array_toString_object", event: "Stop", handler: JSON.stringify({ type: [1, { toString: null }], command: "echo ok" }) },
  { name: "timeout_absent", event: "Stop", handler: handler() },
  { name: "timeout_null", event: "Stop", handler: handler({ timeout: null }) },
  { name: "timeout_600", event: "Stop", handler: handler({ timeout: 600 }) },
  { name: "timeout_0", event: "Stop", handler: handler({ timeout: 0 }) },
  { name: "timeout_0_5", event: "Stop", handler: handler({ timeout: 0.5 }) },
  { name: "timeout_1_5", event: "Stop", handler: handler({ timeout: 1.5 }) },
  { name: "timeout_negative_3", event: "Stop", handler: handler({ timeout: -3 }) },
  { name: "timeout_1e16", event: "Stop", handler: handler({ timeout: 1e16 }) },
  { name: "timeout_1e20", event: "Stop", handler: handler({ timeout: 1e20 }) },
  { name: "timeout_1e21", event: "Stop", handler: handler({ timeout: 1e21 }) },
  { name: "timeout_2p53_plus_1", event: "Stop", handler: handler({ timeout: 9007199254740993 }) },
  { name: "timeout_1e400", event: "Stop", handler: '{"type":"command","command":"echo ok","timeout":1e400}' },
  { name: "timeout_string", event: "Stop", handler: handler({ timeout: "600" }) },
  { name: "async_null", event: "Stop", handler: handler({ async: null }) },
  { name: "async_false", event: "Stop", handler: handler({ async: false }) },
  { name: "async_true", event: "Stop", handler: handler({ async: true }) },
  { name: "async_number", event: "Stop", handler: handler({ async: 1 }) },
  { name: "status_present", event: "Stop", handler: handler({ statusMessage: "Checking" }) },
  { name: "status_null", event: "Stop", handler: handler({ statusMessage: null }) },
  { name: "status_empty", event: "Stop", handler: handler({ statusMessage: "" }) },
  { name: "status_number", event: "Stop", handler: handler({ statusMessage: 5 }) }
];

const recorded = cases.map((testCase) => {
  const parsed = JSON.parse(testCase.handler);
  let answer;
  try {
    answer = { hash: identityHash(testCase.event, testCase.matcher, parsed) };
  } catch (error) {
    answer = { error: error.message };
  }
  const record = { name: testCase.name, event: testCase.event };
  if (testCase.document !== undefined) record.document = testCase.document;
  if (testCase.matcher !== undefined) record.matcher = testCase.matcher;
  return { ...record, handler: testCase.handler, ...answer, canonical: JSON.stringify(parsed) === testCase.handler };
});

const notCanonical = recorded.filter((entry) => !entry.canonical).map((entry) => entry.name);
writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/hook-trust.js",
  node: process.version,
  note: "Each case's handler is the raw JSON document text the oracle's JSON.parse read, so no value is re-encoded; canonical is true when JSON.stringify(JSON.parse(handler)) is that same text.",
  cases: recorded
}, null, 2) + "\n");
console.log(`recorded ${recorded.length} cases to ${out}; not canonical: ${notCanonical.join(", ") || "none"}`);
