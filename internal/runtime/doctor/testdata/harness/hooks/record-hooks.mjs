// Records what CXC v0.2.40's hook observation reader (scripts/hook-observation.mjs readHookObservations)
// and the doctor's hook-execution and hook-trust checks (cxc-ops doctor.ts runHookExecutionCheck,
// runHookTrustCheck) answer over the cases below; harness_hooks_test.go replays testdata/harness/hooks/oracle.json,
// so no Node is needed at test time. Recorded with Node v24.20.0 as
//   node record-hooks.mjs <oracle plugin root, plugins/codexclaw of a read-only v0.2.40 tree> <out file>
// (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
//
// The oracle writer takes its home from the environment, not from an argument, so HOME, CODEX_HOME and
// CRW_HOME are pointed at a temporary root before anything is imported, and every case lives under it; nothing
// outside it is read or written. Records come from the oracle's own recordHookInvocation, then get a fixed
// observedAt (the replay clock is T0) and, for the refusal cases, the patch the case names. A record is stored
// as its text with the case's paths and digests replaced by placeholders, and the replay writes it into the Go
// layout: <codexHome>/crw instead of codexclaw, the manifest as the entrypoint (the Go writer names no component
// script), digests recomputed.
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { lstatSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [pluginArg, outArg] = process.argv.slice(2);
if (!pluginArg || !outArg) throw new Error("usage: node record-hooks.mjs <oracle plugin root> <out file>");
const plugin = resolve(pluginArg);
const out = resolve(outArg);

const temp = mkdtempSync(join(tmpdir(), "cxc-hooks-record-"));
process.env.HOME = temp;
process.env.CODEX_HOME = join(temp, "codex");
process.env.CRW_HOME = join(temp, "crw");
process.chdir(temp);

const observation = await import(join(plugin, "scripts/hook-observation.mjs"));
const doctor = await import(join(plugin, "components/cxc-ops/dist/doctor.js"));
const trust = await import(join(plugin, "components/cxc-ops/dist/hook-trust.js"));

const sha = (value) => createHash("sha256").update(value).digest("hex");
const ENTRY = "components/cxc-ops/src/cli.ts";
const entryOf = (component) => "components/" + component + "/src/cli.ts";
const MANIFEST = '{"version":"1.2.3"}';
const T0 = Date.parse("2026-01-01T00:00:00.000Z");
const DAY = 24 * 60 * 60 * 1000;
const actorDir = (c, session, agent) => join(c.codexHome, "codexclaw", "hook-observations", sha(session), sha(JSON.stringify(agent)));
const slotName = (slot) => sha(JSON.stringify(slot)) + ".json";

let counter = 0;
function fixture(name, mutation = null) {
  const root = join(temp, "c" + String(++counter));
  const c = { name, mutation, root, codexHome: join(root, "home"), plugin: join(root, "plugin"), entry: join(root, "plugin", ENTRY), dirs: new Map(), slots: new Map() };
  mkdirSync(join(c.plugin, ".codex-plugin"), { recursive: true });
  mkdirSync(join(c.plugin, "components/cxc-ops/src"), { recursive: true });
  writeFileSync(join(c.plugin, ".codex-plugin/plugin.json"), MANIFEST);
  writeFileSync(c.entry, "// fixture");
  return c;
}
// One record by the oracle writer, time pinned to T0; returns its handle.
function record(c, { component = "cxc-ops", event = "session-start", session = "s1", agent = null } = {}) {
  process.env.CODEX_HOME = c.codexHome;
  const payload = JSON.stringify({ session_id: session, ...(agent === null ? {} : { agent_id: agent, agent_type: "executor" }) });
  const script = join(c.plugin, entryOf(component));
  mkdirSync(join(script, ".."), { recursive: true });
  writeFileSync(script, "// fixture");
  if (!observation.recordHookInvocation(payload, component, event, script)) throw new Error("the oracle did not record " + event);
  const slot = [component, event, entryOf(component)];
  const dir = actorDir(c, session, agent);
  const file = join(dir, slotName(slot));
  const value = JSON.parse(readFileSync(file, "utf8"));
  value.observedAt = "2026-01-01T00:00:00.000Z";
  writeFileSync(file, JSON.stringify(value) + "\n");
  c.dirs.set(dir, { session, agent });
  c.slots.set(slotName(slot), slot);
  return { file, dir, slot, session, agent };
}
const patch = (h, changes) => writeFileSync(h.file, JSON.stringify({ ...JSON.parse(readFileSync(h.file, "utf8")), ...changes }));
const dropKey = (h, key) => { const value = JSON.parse(readFileSync(h.file, "utf8")); delete value[key]; writeFileSync(h.file, JSON.stringify(value)); };
const rewrite = (h, edit) => writeFileSync(h.file, edit(readFileSync(h.file, "utf8")));
const extraSlot = (c, h, event) => { const slot = ["cxc-ops", event, ENTRY]; c.slots.set(slotName(slot), slot); return join(h.dir, slotName(slot)); };

function mutate(c) {
  const manifest = join(c.plugin, ".codex-plugin/plugin.json");
  if (c.mutation === "entrypoint") writeFileSync(c.entry, "// changed target");
  if (c.mutation === "manifest_extra") writeFileSync(manifest, '{"version":"1.2.3","hooks":[]}');
  if (c.mutation === "manifest_missing") unlinkSync(manifest);
  if (c.mutation === "manifest_noversion") writeFileSync(manifest, '{"name":"x"}');
  if (c.mutation === "manifest_symlink") { writeFileSync(join(c.root, "real.json"), MANIFEST); unlinkSync(manifest); symlinkSync(join(c.root, "real.json"), manifest); }
  if (c.mutation === "ancestor_file") { rmSync(join(c.codexHome, "codexclaw"), { recursive: true }); writeFileSync(join(c.codexHome, "codexclaw"), "not a directory"); }
}
function normalise(c, text) {
  return text.replaceAll(c.plugin, "@@PLUGIN_ROOT@@").replaceAll(sha(MANIFEST), "@@MANIFEST_DIGEST@@").replaceAll(sha("// fixture"), "@@ENTRY_DIGEST@@");
}
const readlinkName = (path) => execFileSync("readlink", [path], { encoding: "utf8" }).trim().split("/").pop();
// What each actor directory holds, before the case's mutation.
function snapshot(c) {
  const files = [];
  for (const [dir, actor] of c.dirs) {
    for (const name of readdirSync(dir).sort()) {
      const path = join(dir, name);
      const stat = lstatSync(path);
      const entry = { actor, ...(c.slots.has(name) ? { slot: c.slots.get(name) } : { name }) };
      if (stat.isSymbolicLink()) entry.symlinkTo = c.slots.get(readlinkName(path));
      else if (stat.isDirectory()) entry.dir = true;
      else if (stat.isFIFO()) entry.fifo = true;
      else {
        // A record of exactly the read bound is stored without its padding: a path of another length changes its size.
        const raw = readFileSync(path, "utf8");
        entry.text = normalise(c, raw);
        if (Buffer.byteLength(raw) === 8192) { entry.padTo = 8192; entry.text = entry.text.replace(/ +\}$/, "}"); }
      }
      files.push(entry);
    }
  }
  return files;
}
const plain = (value) => JSON.parse(JSON.stringify(value));

// A query is answered by the reader and by the doctor check over the same options.
function answer(c, q) {
  const fromEnv = q.sessionFromEnv !== undefined;
  const reader = fromEnv ? null : observation.readHookObservations({ pluginRoot: c.plugin, codexHome: c.codexHome, sessionId: q.sessionId, agentId: q.agentId ?? null,
    now: T0 + (q.nowOffsetMs ?? 0), maxAgeMs: q.maxAgeMs ?? undefined });
  if (fromEnv) process.env.CODEX_THREAD_ID = q.sessionFromEnv;
  const options = { codexHome: c.codexHome, agentId: q.agentId ?? null, observationNow: T0 + (q.nowOffsetMs ?? 0), observationMaxAgeMs: q.maxAgeMs ?? undefined };
  if (!fromEnv) options.sessionId = q.sessionId;
  const check = doctor.runHookExecutionCheck(c.plugin, options);
  delete process.env.CODEX_THREAD_ID;
  return { ...q, reader: reader && plain(reader), check: plain(check) };
}
const readerCases = [];
function add(c, queries) {
  const files = snapshot(c);
  mutate(c);
  readerCases.push({ name: c.name, mutation: c.mutation, files, queries: queries.map((q) => answer(c, q)) });
}
const root = { sessionId: "s1", agentId: null };

{ const c = fixture("valid_root"); record(c); add(c, [root, { sessionId: "s1", agentId: null, maxAgeMs: 0 }, { sessionFromEnv: "s1" }, { sessionId: "s1", agentId: "child-a" }, { sessionId: "other", agentId: null }]); }
{ const c = fixture("actors_and_events_are_isolated");
  record(c); record(c, { event: "post-compact" }); record(c, { agent: "child-a" }); record(c, { session: "s2" });
  add(c, [root, { sessionId: "s1", agentId: "child-a" }, { sessionId: "s1", agentId: "child-b" }, { sessionId: "s2", agentId: null }]); }
{ const c = fixture("sorted_by_event_in_byte_order");
  for (const event of ["stop", "session-start", "post-compact", "a1", "a-b", "ab", "a", "z9", "z-9", "a-1"]) record(c, { event });
  add(c, [root]); }
{ const c = fixture("identity_unavailable"); record(c);
  add(c, [{ sessionId: "", agentId: null }, { sessionId: "s1", agentId: "" }, { sessionId: "s1\nx", agentId: null }, { sessionId: "s1", agentId: "a b" }]); }
{ const c = fixture("freshness_window"); record(c);
  add(c, [{ ...root, nowOffsetMs: DAY }, { ...root, nowOffsetMs: DAY + 1 }, { ...root, nowOffsetMs: -1 }, { ...root, maxAgeMs: -1 }, { ...root, maxAgeMs: 0 }, { ...root, nowOffsetMs: 1, maxAgeMs: 0 }]); }
const patches = [
  ["schemaVersion_2", { schemaVersion: 2 }], ["schemaVersion_text", { schemaVersion: "1" }], ["outcome_other", { outcome: "success" }],
  ["session_foreign", { sessionId: "foreign" }], ["agent_set", { agentId: "child" }], ["plugin_root_foreign", { pluginRoot: "/foreign" }],
  ["plugin_version_old", { pluginVersion: "old" }], ["manifest_digest_zeros", { manifestDigest: "0".repeat(64) }],
  ["entrypoint_digest_zeros", { entrypointDigest: "0".repeat(64) }], ["entrypoint_escapes", { entrypoint: "../escape.ts" }],
  ["entrypoint_other_script", { entrypoint: "components/cxc-ops/src/other.ts" }], ["component_not_a_slug", { component: "../escape" }],
  ["event_not_a_slug", { event: "Bad Event" }], ["observed_at_garbage", { observedAt: "nonsense" }], ["observed_at_number", { observedAt: 5 }],
  ["observed_at_in_the_future", { observedAt: new Date(T0 + 1).toISOString() }], ["observed_at_too_old", { observedAt: new Date(T0 - DAY - 1).toISOString() }],
  ["extra_fields_are_not_projected", { prompt: "PRIVATE", tool_input: { secret: 1 } }],
];
for (const [name, changes] of patches) { const c = fixture("record_" + name); patch(record(c), changes); add(c, [root]); }
{ const c = fixture("record_schemaVersion_1_point_0"); rewrite(record(c), (text) => text.replace('"schemaVersion":1,', '"schemaVersion":1.0,')); add(c, [root]); }
{ const c = fixture("record_agentId_key_removed"); dropKey(record(c), "agentId"); add(c, [root]); }
{ const c = fixture("record_sessionId_key_removed"); dropKey(record(c), "sessionId"); add(c, [root]); }
{ const c = fixture("record_pluginRoot_key_removed"); dropKey(record(c), "pluginRoot"); add(c, [root]); }
{ const c = fixture("record_not_an_object"); const h = record(c); writeFileSync(h.file, "[]"); add(c, [root]); }
{ const c = fixture("record_json_null"); const h = record(c); writeFileSync(h.file, "null"); add(c, [root]); }
{ const c = fixture("record_corrupt"); const h = record(c); writeFileSync(h.file, "{"); add(c, [root]); }
{ const c = fixture("record_over_8192_bytes"); const h = record(c); writeFileSync(h.file, "x".repeat(9000)); add(c, [root]); }
{ const c = fixture("record_at_8192_bytes_is_read"); const h = record(c); const text = readFileSync(h.file, "utf8"); writeFileSync(h.file, text.slice(0, -2) + " ".repeat(8192 - text.length + 1) + "}"); add(c, [root]); }
{ const c = fixture("record_wrong_slot"); const h = record(c); writeFileSync(extraSlot(c, h, "post-compact"), readFileSync(h.file)); add(c, [root]); }
{ const c = fixture("stray_names_are_skipped_and_not_counted"); const h = record(c);
  for (const name of ["notes.txt", ".tmp", "A".repeat(64) + ".json", "a".repeat(63) + ".json", "a".repeat(64) + ".jsonl"]) writeFileSync(join(h.dir, name), "x");
  add(c, [root]); }
{ const c = fixture("slot_named_directory"); const h = record(c); mkdirSync(extraSlot(c, h, "stop")); add(c, [root]); }
{ const c = fixture("slot_named_symlink"); const h = record(c); symlinkSync(h.file, extraSlot(c, h, "stop")); add(c, [root]); }
{ const c = fixture("slot_named_fifo"); const h = record(c); execFileSync("mkfifo", [extraSlot(c, h, "stop")]); add(c, [root]); }
{ const c = fixture("entrypoint_changed", "entrypoint"); record(c); add(c, [root]); }
{ const c = fixture("manifest_changed", "manifest_extra"); record(c); add(c, [root]); }
{ const c = fixture("manifest_missing", "manifest_missing"); record(c); add(c, [root]); }
{ const c = fixture("manifest_without_version", "manifest_noversion"); record(c); add(c, [root]); }
{ const c = fixture("manifest_is_a_symlink", "manifest_symlink"); record(c); add(c, [root]); }
{ const c = fixture("no_records_for_the_actor"); add(c, [root, { sessionId: "s1", agentId: "child-a" }]); }
{ const c = fixture("store_path_is_a_file", "ancestor_file"); record(c); add(c, [root]); }

// Hook trust: runHookTrustCheck over a plugin and a config.toml. A hook file and the config text are literal;
// the keys and hashes in them come from the oracle's own listHookEntries.
const handler = (command, extra = {}) => ({ type: "command", command, ...extra });
const trustCases = [];
function trustCase(name, { manifest = { name: "fixture", hooks: ["./hooks/sample.json"] }, hooks = {}, config = null, key = undefined, env = false, entriesKey = "fixture@market", engineError = false } = {}) {
  const base = join(temp, "t" + String(++counter));
  const pluginRoot = join(base, "plugin");
  const codexHome = join(base, "home");
  mkdirSync(join(pluginRoot, ".codex-plugin"), { recursive: true });
  mkdirSync(join(pluginRoot, "hooks"), { recursive: true });
  mkdirSync(codexHome, { recursive: true });
  const manifestText = manifest === null || typeof manifest === "string" ? manifest : JSON.stringify(manifest);
  if (manifest !== null) writeFileSync(join(pluginRoot, ".codex-plugin/plugin.json"), manifestText);
  const files = {};
  for (const [file, doc] of Object.entries(hooks)) { files[file] = JSON.stringify(doc); writeFileSync(join(pluginRoot, file), files[file]); }
  let entries = [];
  try { entries = trust.listHookEntries(pluginRoot, entriesKey); } catch { /* the case holds an unreadable plugin */ }
  const configText = typeof config === "function" ? config(entries) : config;
  if (configText !== null) writeFileSync(join(codexHome, "config.toml"), configText);
  const options = env ? {} : { codexHome };
  if (key !== undefined) options.pluginKey = key;
  if (env) process.env.CODEX_HOME = codexHome;
  const check = plain(doctor.runHookTrustCheck(pluginRoot, options));
  const own = (text) => text.replaceAll(codexHome, "@@CODEX_HOME@@");
  trustCases.push({ name, manifest: manifestText, files, config: configText, key: key ?? null, codexHomeFromEnv: env,
    expect: { severity: check.severity, evidence: engineError ? null : own(check.evidence), ...(check.repair === undefined ? {} : { repair: own(check.repair) }) } });
}
const section = (entry, hash = entry.hash) => '[hooks.state."' + entry.key + '"]\ntrusted_hash = "' + hash + '"\n';
const enabled = (key) => '[plugins."' + key + '"]\nenabled = true\n';
const stop = { hooks: { Stop: [{ hooks: [handler("echo doctor")] }] } };
const three = { hooks: { Stop: [{ hooks: [handler("echo trusted")] }, { hooks: [handler("echo drifted")] }, { hooks: [handler("echo untrusted")] }] } };
const both = { hooks: { Stop: [{ hooks: [handler("echo drifted")] }, { hooks: [handler("echo missing")] }] } };
const sample = { "hooks/sample.json": stop };
trustCase("ambiguous_two_install_keys", { hooks: sample, config: enabled("fixture@one") + enabled("fixture@two") });
trustCase("no_config_and_no_key", { hooks: sample });
trustCase("no_install_key_but_option_names_one_with_no_config", { hooks: sample, key: "fixture@market" });
trustCase("never_trusted_names_the_bootstrap_repair", { hooks: sample, config: enabled("fixture@market"), key: "fixture@market" });
trustCase("drift_with_explicit_key", { hooks: sample, entriesKey: "fixture@one", key: "fixture@one", config: (e) => section(e[0], "sha256:stale") });
trustCase("drift_repair_omits_bootstrap", { hooks: sample, key: "fixture@market", config: (e) => section(e[0], "sha256:stale") });
trustCase("trusted_untrusted_drifted_listed", { hooks: { "hooks/sample.json": three }, key: "fixture@market", config: (e) => section(e[0]) + "\n" + section(e[1], "sha256:stale") });
trustCase("drift_mixed_with_missing", { hooks: { "hooks/sample.json": both }, key: "fixture@market", config: (e) => section(e[0], "sha256:stale") });
trustCase("fully_trusted_has_no_repair", { hooks: sample, key: "fixture@market", config: (e) => section(e[0]) });
trustCase("single_enabled_install_key_is_used", { hooks: sample, config: (e) => enabled("fixture@market") + section(e[0]) });
trustCase("disabled_install_key_is_not_a_candidate", { hooks: sample, config: (e) => '[plugins."fixture@old"]\nenabled = false\n' + enabled("fixture@market") + section(e[0]) });
trustCase("several_install_keys_option_picks_one", { hooks: sample, key: "fixture@market", config: (e) => enabled("fixture@market") + enabled("fixture@other") + section(e[0]) });
trustCase("codex_home_from_the_environment", { hooks: sample, env: true, config: (e) => enabled("fixture@market") + section(e[0]) });
trustCase("no_handler_can_be_hashed", { hooks: { "hooks/sample.json": { hooks: { Stop: [{ hooks: [handler("echo x", { async: true })] }] } } }, key: "fixture@market", config: enabled("fixture@market") });
trustCase("manifest_without_hooks_lists_nothing", { manifest: { name: "fixture" }, key: "fixture@market", config: enabled("fixture@market") });
trustCase("manifest_without_a_name", { manifest: { hooks: [] } });
trustCase("manifest_with_an_empty_name", { manifest: { name: "" } });
trustCase("manifest_name_not_a_string", { manifest: { name: 7 } });
trustCase("manifest_is_json_null", { manifest: "null", engineError: true });
trustCase("manifest_missing", { manifest: null, engineError: true });
trustCase("manifest_is_not_json", { manifest: "{", engineError: true });
trustCase("hook_file_missing", { hooks: {}, key: "fixture@market", config: enabled("fixture@market"), engineError: true });

writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  node: process.version,
  t0: T0,
  entry: ENTRY,
  manifest: MANIFEST,
  note: "reader holds one case per record or store state: the files the oracle writer and the case's patch left in the actor directories, as text with @@PLUGIN_ROOT@@, @@MANIFEST_DIGEST@@ and @@ENTRY_DIGEST@@ placeholders, then each query with the oracle readHookObservations answer (reader) and runHookExecutionCheck answer (check). Time is t0; nowOffsetMs moves it. trust holds runHookTrustCheck over a plugin and config.toml, @@CODEX_HOME@@ standing for the case's codex home; evidence null marks an engine error text (a V8 or Node message) the Go port cannot reproduce, of which only the severity is held.",
  reader: readerCases,
  trust: trustCases,
}, null, 1) + "\n");
console.log("recorded " + readerCases.length + " reader and " + trustCases.length + " trust cases to " + out);
rmSync(temp, { recursive: true, force: true });
