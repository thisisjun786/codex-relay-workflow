// Records what CXC v0.2.40's readInstalledPluginKeys and diagnoseHookTrust answer over the
// config.toml cases below; the Go test replays config-oracle.json (no Node at test time).
// Recorded with Node v24 as
//   node record-config.mjs <oracle cxc-ops dist dir> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d). A case is one plugin tree (a hook document at
// hooks/sample.json under a manifest that declares it) and a config.toml (text, {base64} for
// bytes, or absent), with the oracle's answers: the keys readInstalledPluginKeys returns, and
// one answer per diagnoseHookTrust result (key, status, actual). Structured errors are recorded
// by text and engine errors (a read that fails) by class, because their text names a host path
// or the engine.
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [dist, out] = process.argv.slice(2);
if (!dist || !out) throw new Error("usage: node record-config.mjs <oracle cxc-ops dist dir> <out file>");
const { diagnoseHookTrust, listHookEntries, readInstalledPluginKeys } = await import(resolve(dist, "hook-trust.js"));

const KEY = "fixture@market";
const MANIFEST = ".codex-plugin/plugin.json";
const stopDocument = (command = "echo ok") => JSON.stringify({ hooks: { Stop: [{ hooks: [{ type: "command", command }] }] } });
const twoGroupDocument = JSON.stringify({
  hooks: {
    PreToolUse: [
      { hooks: [{ type: "command", command: "a" }] },
      { matcher: "^x$", hooks: [{ type: "command", command: "b" }] },
    ],
  },
});
const bytes = (...parts) => ({ base64: Buffer.concat(parts.map((part) => (Buffer.isBuffer(part) ? part : Buffer.from(part)))).toString("base64") });

// Each case: a name, an optional document (defaults to one Stop command hook), an optional key,
// an optional pluginName for readInstalledPluginKeys, and a config (text, {base64}, a function of
// the case's first entry, or null for no config.toml).
const section = (entry, hash = entry.hash) => `[hooks.state."${entry.key}"]\ntrusted_hash = "${hash}"\n`;
const cases = [
  { name: "missing_config", config: null },
  { name: "empty_config", config: "" },
  { name: "enabled_candidates_and_disabled_sections", config: [
    '[plugins."fixture@one"]',
    "enabled = true",
    '[plugins."fixture@off"]',
    "enabled = false # intentionally disabled",
    '[plugins."other@market"]',
    "enabled = true",
    '[plugins."fixture@two"]',
    'source = "dev"',
    "",
  ].join("\n") },
  { name: "trailing_comments_on_headers", config: (entry) => [
    '[plugins."fixture@market"] # installed from local marketplace',
    "enabled = true",
    `[hooks.state."${entry.key}"]  # retained by operator`,
    `trusted_hash = "${entry.hash}" # written by Codex`,
    "",
  ].join("\n") },
  { name: "crlf_trusted", config: (entry) => [
    '[plugins."fixture@market"]',
    "enabled = true",
    `[hooks.state."${entry.key}"]`,
    `trusted_hash = "${entry.hash}"`,
    "",
  ].join("\r\n") },
  { name: "crlf_disabled_section", config: [
    '[plugins."fixture@market"]',
    "enabled = false",
    "",
  ].join("\r\n") },
  { name: "duplicate_headers", config: (entry) => section(entry) + "\n" + section(entry) },
  { name: "two_trusted_hash_lines", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = "${entry.hash}"\ntrusted_hash = "sha256:stale"\n` },
  { name: "empty_trusted_hash_value", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = ""\n` },
  { name: "escaped_quote_in_value_is_no_record", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = "a\\"b"\n` },
  { name: "escaped_quote_before_section", config: (entry) =>
    'note = "say \\"hi\\""\n' + section(entry) },
  { name: "fake_section_in_basic_multiline", config: (entry) => [
    'payload = """',
    `[hooks.state."${entry.key}"]`,
    `trusted_hash = "${entry.hash}"`,
    '"""',
    "",
  ].join("\n") },
  { name: "fake_section_then_real_section_basic", config: (entry) => [
    'payload = """',
    `[hooks.state."${entry.key}"]`,
    `trusted_hash = "sha256:stale"`,
    '"""',
    `[hooks.state."${entry.key}"]`,
    `trusted_hash = "${entry.hash}"`,
    "",
  ].join("\n") },
  { name: "fake_section_in_literal_multiline", config: (entry) => [
    "plugin_note = '''",
    `[hooks.state."${entry.key}"]`,
    `trusted_hash = "${entry.hash}"`,
    "'''",
    '[plugins."fixture@market"]',
    "enabled = true",
    "plugin_note = '''",
    "enabled = false # string content must not disable the plugin",
    "'''",
    "",
  ].join("\n") },
  { name: "escaped_close_keeps_basic_multiline_open", config: (entry) => [
    'payload = """',
    '\\"""',
    `[hooks.state."${entry.key}"]`,
    `trusted_hash = "${entry.hash}"`,
    '"""',
    "",
  ].join("\n") },
  // The dot of a comment in the oracle's patterns is JavaScript's, which excludes CR, U+2028
  // and U+2029 as well as LF; a comment holding one of those is no comment, so the line is no
  // trusted_hash record (or no section header, or no enabled = false).
  { name: "comment_line_separator_after_trusted_hash", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = "${entry.hash}" #x\u2028y\n` },
  { name: "comment_carriage_return_after_trusted_hash", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = "${entry.hash}" #x\ry\n` },
  { name: "comment_line_separator_on_section_header", config: (entry) =>
    `[hooks.state."${entry.key}"] # x\u2028y\ntrusted_hash = "${entry.hash}"\n` },
  { name: "comment_line_separator_on_enabled_false", config:
    `[plugins."fixture@one"]\nenabled = false # x\u2028y\n` },
  { name: "invalid_utf8_before_section", config: (entry) => bytes("\ufffd = ", Buffer.from([0xff, 0xfe]), "\n", section(entry)) },
  { name: "multibyte_before_section", config: (entry) => bytes("\u00ff = \u00fe\n" + section(entry)) },
  { name: "config_is_directory", configDir: true },
  // A file-final CR (or U+2028) stays in the last line's text, and no side accepts the line:
  // JavaScript's $ has no before-a-final-terminator rule (the review thread questioned this).
  { name: "final_cr_after_plugin_header", config: '[plugins."fixture@one"]\r' },
  { name: "final_cr_after_enabled_false", config: '[plugins."fixture@one"]\nenabled = false\r' },
  { name: "final_cr_after_trusted_hash", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = "${entry.hash}"\r` },
  { name: "final_u2028_after_trusted_hash", config: (entry) =>
    `[hooks.state."${entry.key}"]\ntrusted_hash = "${entry.hash}"\u2028` },
  { name: "bom_before_first_plugin_header", config: (entry) =>
    "\ufeff[plugins.\"fixture@market\"]\nenabled = true\n" + section(entry) },
  { name: "cr_only_line_endings", config: (entry) => section(entry).replace(/\n/g, "\r") },
  { name: "plugin_name_is_escaped", pluginName: "a+b", config: [
    '[plugins."aab@one"]',
    "enabled = true",
    '[plugins."a+b@two"]',
    "enabled = true",
    "",
  ].join("\n") },
  { name: "duplicate_enabled_sections_kept", config: [
    '[plugins."fixture@one"]',
    "enabled = true",
    '[plugins."fixture@one"]',
    "enabled = 1",
    "",
  ].join("\n") },
  { name: "two_groups_second_trusted", document: twoGroupDocument, entryIndex: 1,
    config: (entry) => section(entry) },
];

const answerOf = (error) => {
  if (error.code) return { errorClass: error.code };
  if (error.name === "SyntaxError") return { errorClass: "SyntaxError" };
  return { error: error.message };
};

const recorded = [];
for (const testCase of cases) {
  const base = mkdtempSync(join(tmpdir(), "htc-"));
  const root = join(base, "plugin");
  const home = join(base, "home");
  try {
    mkdirSync(join(root, ".codex-plugin"), { recursive: true });
    mkdirSync(join(root, "hooks"), { recursive: true });
    mkdirSync(home, { recursive: true });
    writeFileSync(join(root, MANIFEST), JSON.stringify({ name: "fixture", hooks: ["./hooks/sample.json"] }));
    writeFileSync(join(root, "hooks", "sample.json"), testCase.document ?? stopDocument());
    const entry = listHookEntries(root, testCase.key ?? KEY)[testCase.entryIndex ?? 0];
    const config = typeof testCase.config === "function" ? testCase.config(entry) : testCase.config;
    const answer = { name: testCase.name };
    if (testCase.document !== undefined) answer.document = testCase.document;
    if (testCase.key !== undefined) answer.key = testCase.key;
    if (testCase.pluginName !== undefined) answer.pluginName = testCase.pluginName;
    if (testCase.configDir) {
      mkdirSync(join(home, "config.toml"), { recursive: true });
      answer.configDir = true;
    } else if (config === null || config === undefined) answer.noConfig = true;
    else if (typeof config === "string") answer.config = config;
    else answer.configBase64 = config.base64;
    if (!testCase.configDir && config !== null && config !== undefined) {
      writeFileSync(join(home, "config.toml"), typeof config === "string" ? config : Buffer.from(config.base64, "base64"));
    }
    try {
      answer.keys = readInstalledPluginKeys(home, testCase.pluginName ?? "fixture");
    } catch (error) {
      const reported = answerOf(error);
      if (reported.errorClass) answer.keysErrorClass = reported.errorClass;
      else answer.keysError = reported.error;
    }
    try {
      answer.diagnose = diagnoseHookTrust(home, root, testCase.key ?? KEY).map((item) => ({ key: item.key, status: item.status, actual: item.actual }));
    } catch (error) {
      const reported = answerOf(error);
      if (reported.errorClass) answer.diagnoseErrorClass = reported.errorClass;
      else answer.diagnoseError = reported.error;
    }
    recorded.push(answer);
  } finally {
    rmSync(base, { recursive: true, force: true });
  }
}

const names = new Set();
for (const item of recorded) {
  if (names.has(item.name)) throw new Error("duplicate case " + item.name);
  names.add(item.name);
}
writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/hook-trust.js",
  node: process.version,
  note: "Each case builds one plugin (a Stop or PreToolUse document at hooks/sample.json) and a config.toml (text, {base64} bytes, or none), then records readInstalledPluginKeys keys and one diagnoseHookTrust answer per entry (key, status, actual); a read that fails is recorded as error or errorClass.",
  cases: recorded,
}, null, 1) + "\n");
console.log("recorded " + recorded.length + " cases to " + out);
