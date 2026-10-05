// Records what CXC v0.2.40's listHookEntries answers over the plugin trees below; the Go test
// replays entries-oracle.json (no Node at test time). Recorded with Node v24 as
//   node record-entries.mjs <oracle cxc-ops dist dir> <oracle plugin dir> <repo root> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d) and the plugin dir is that tree's plugins/codexclaw.
// A case is a tree of files and symlinks under one base directory; its plugin root is base/plugin
// unless the case names another. Structured oracle errors are recorded by text, engine errors
// (ENOENT, EISDIR, JSON SyntaxError) by class, because their text names a host path or the engine.
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";

const [dist, oraclePlugin, root, out] = process.argv.slice(2);
if (!dist || !oraclePlugin || !root || !out) throw new Error("usage: node record-entries.mjs <oracle dist dir> <oracle plugin dir> <repo root> <out file>");
const { listHookEntries } = await import(resolve(dist, "hook-trust.js"));

const KEY = "fixture@market";
const h = (extra = {}) => ({ type: "command", command: "echo ok", ...extra });
const g = (hooks, matcher) => (matcher === undefined ? { hooks } : { matcher, hooks });
const MANIFEST = ".codex-plugin/plugin.json";
const file = (rel, content) => ({ ["plugin/" + rel]: typeof content === "string" || content?.base64 !== undefined ? content : JSON.stringify(content) });
// one plugin whose manifest names refs and whose hooks/a.json holds doc (an object or raw text)
const plug = (name, doc, o = {}) => ({
  name, key: o.key, relativeRoot: o.relativeRoot,
  files: { ...file(MANIFEST, o.manifest ?? { name: "fixture", hooks: o.refs ?? ["./hooks/a.json"] }), ...file("hooks/a.json", doc), ...Object.fromEntries(Object.entries(o.files ?? {}).map(([rel, content]) => [rel, typeof content === "string" || content?.base64 !== undefined ? content : JSON.stringify(content)])) },
  links: o.links,
});
const ev = (event, groups) => ({ hooks: { [event]: groups } });
const stop = (handler) => ev("Stop", [g([handler])]);
const bytes = (...parts) => ({ base64: Buffer.concat(parts.map((p) => (typeof p === "string" ? Buffer.from(p) : Buffer.from(p)))).toString("base64") });
const withCommand = (command) => bytes('{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"', command, '"}]}]}}');
const matcher = (name, m) => plug(name, ev("PreToolUse", [g([h()], m)]));

const protoEvents = ["constructor", "toString", "valueOf", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "toLocaleString", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__"];
const labels = ["PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "Stop", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact", "PermissionRequest"];

const oracleManifest = readFileSync(join(oraclePlugin, MANIFEST), "utf8");
const oracleFiles = Object.fromEntries(JSON.parse(oracleManifest).hooks.map((ref) => ["plugin/" + ref.replace(/^\.\//, ""), readFileSync(join(oraclePlugin, ref), "utf8")]));
const crwManifest = readFileSync(join(root, "plugins", "crw", MANIFEST), "utf8");
const crwFiles = Object.fromEntries(JSON.parse(crwManifest).hooks.map((ref) => ["plugin/" + ref.replace(/^\.\//, ""), readFileSync(join(root, "plugins", "crw", ref), "utf8")]));

const cases = [
  { name: "oracle_codexclaw_plugin", key: "codexclaw@local", files: { ...file(MANIFEST, oracleManifest), ...oracleFiles } },
  { name: "crw_plugin_snapshot", key: "crw@local", files: { ...file(MANIFEST, crwManifest), ...crwFiles } },
  plug("all_ten_events", { hooks: Object.fromEntries(labels.map((l) => [l, [g([h()], "^m$")]])) }),
  plug("event_order_unsorted", { hooks: { Stop: [g([h()])], PreToolUse: [g([h()])], SessionStart: [g([h()])] } }),
  plug("duplicate_event_key", '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"first"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"pre"}]}],"Stop":[{"hooks":[{"type":"command","command":"last"}]}]}}'),
  plug("two_files_in_manifest_order", ev("Stop", [g([h({ command: "a" })])]), { refs: ["./hooks/b.json", "./hooks/a.json"], files: file("hooks/b.json", ev("PreToolUse", [g([h({ command: "b" })])])) }),
  plug("later_ref_failure_discards_earlier", ev("Stop", [g([h()])]), { refs: ["./hooks/a.json", "../outside/x.json"] }),
  plug("ref_dot_slash", ev("Stop", [g([h()])]), { refs: ["./hooks/a.json"] }),
  plug("ref_double_dot_slash", ev("Stop", [g([h()])]), { refs: ["././hooks/a.json"] }),
  plug("ref_no_prefix", ev("Stop", [g([h()])]), { refs: ["hooks/a.json"] }),
  plug("ref_triple_dot_slash_then_slash", ev("Stop", [g([h()])]), { refs: ["././/hooks/a.json"] }),
  plug("ref_triple_dot_slash_then_absolute", ev("Stop", [g([h()])]), { refs: ["././/etc/hosts"] }),
  plug("ref_lone_surrogate_path", ev("Stop", [g([h()])]), { manifest: '{"hooks":["hooks/\\ud800.json"]}', files: { "plugin/hooks/\ufffd.json": ev("Stop", [g([h()])]) } }),
  plug("ref_sibling_prefix_escape", ev("Stop", [g([h()])]), { refs: ["../plugin-sibling/a.json"], files: { "plugin-sibling/a.json": ev("Stop", [g([h()])]) } }),
  plug("ref_symlink_to_sibling_prefix", ev("Stop", [g([h()])]), { refs: ["sib/a.json"], files: { "plugin-sibling/a.json": ev("Stop", [g([h()])]) }, links: { "plugin/sib": "../plugin-sibling" } }),
  plug("manifest_duplicate_refs", ev("Stop", [g([h()])]), { refs: ["./hooks/a.json", "./hooks/a.json"] }),
  plug("async_true_with_invalid_timeout_skipped", stop(h({ async: true, timeout: "bad" }))),
  matcher("matcher_octal_range_end_kept", "[\\1-Z]"),
  matcher("matcher_octal_range_hex_end_kept", "^[\\1-\\x20]+$"),
  matcher("matcher_class_range_to_octal_skipped", "[a-\\1]"),
  matcher("matcher_escaped_backslash_then_lookahead_kept", "\\\\(?=a)"),
  matcher("matcher_residual_reversed_octal_range", "[\\3-\\1]"),
  matcher("matcher_residual_unknown_named_backreference", "(?<n>a)\\k<x>"),
  matcher("matcher_residual_quantified_lookbehind", "(?<=a)*"),
  matcher("matcher_residual_class_range_to_decimal_escape", "[0-\\9]"),
  matcher("matcher_residual_class_octal_range", "[\\x02-\\7]"),
  matcher("matcher_residual_class_range_to_named_escape", "[a-\\k<z>]"),
  plug("ref_dotdot_inside_root", ev("Stop", [g([h()])]), { refs: ["hooks/../hooks/a.json"] }),
  plug("relative_plugin_root", ev("Stop", [g([h()])]), { relativeRoot: true }),
  { ...plug("plugin_root_is_symlink", ev("Stop", [g([h()])])), rootName: "link", links: { link: "plugin" } },
  plug("plugin_key_empty", ev("Stop", [g([h()])]), { key: "" }),
  plug("plugin_key_quote", ev("Stop", [g([h()])]), { key: 'a"b' }),
  plug("plugin_key_backslash", ev("Stop", [g([h()])]), { key: "a\\b" }),
  plug("plugin_key_newline", ev("Stop", [g([h()])]), { key: "a\nb" }),
  plug("plugin_key_carriage_return", ev("Stop", [g([h()])]), { key: "a\rb" }),
  plug("hook_path_backslash", ev("Stop", [g([h()])]), { refs: ["hooks\\a.json"] }),
  plug("hook_path_quote", ev("Stop", [g([h()])]), { refs: ['hooks/"a.json'] }),
  plug("hook_path_newline", ev("Stop", [g([h()])]), { refs: ["hooks/a\n.json"] }),
  plug("hook_path_empty", ev("Stop", [g([h()])]), { refs: [""] }),
  plug("hook_path_only_dot_slash", ev("Stop", [g([h()])]), { refs: ["./"] }),
  plug("ref_absolute", ev("Stop", [g([h()])]), { refs: ["/etc/hosts"] }),
  plug("ref_absolute_after_dot_slash", ev("Stop", [g([h()])]), { refs: [".//etc/hosts"] }),
  plug("ref_dotdot_escape", ev("Stop", [g([h()])]), { refs: ["../outside/x.json"], files: { "outside/x.json": "{}" } }),
  plug("ref_nested_dotdot_escape", ev("Stop", [g([h()])]), { refs: ["hooks/../../outside/x.json"], files: { "outside/x.json": "{}" } }),
  plug("ref_symlink_file_outside", ev("Stop", [g([h()])]), { refs: ["hooks/link.json"], files: { "outside/x.json": ev("Stop", [g([h()])]) }, links: { "plugin/hooks/link.json": "../../outside/x.json" } }),
  plug("ref_symlink_dir_outside", ev("Stop", [g([h()])]), { refs: ["linked/x.json"], files: { "outside/x.json": ev("Stop", [g([h()])]) }, links: { "plugin/linked": "../outside" } }),
  plug("ref_symlink_inside_root", ev("Stop", [g([h()])]), { refs: ["hooks/link.json"], links: { "plugin/hooks/link.json": "a.json" } }),
  plug("ref_missing_file", ev("Stop", [g([h()])]), { refs: ["hooks/missing.json"] }),
  plug("ref_is_directory", ev("Stop", [g([h()])]), { refs: ["hooks"] }),
  plug("ref_dot", ev("Stop", [g([h()])]), { refs: ["."] }),
  { name: "root_missing", files: {} },
  plug("manifest_hooks_absent", {}, { manifest: { name: "fixture" } }),
  plug("manifest_hooks_empty_array", {}, { manifest: { hooks: [] } }),
  plug("manifest_hooks_object", {}, { manifest: { hooks: { a: "./hooks/a.json" } } }),
  plug("manifest_hooks_string", {}, { manifest: { hooks: "./hooks/a.json" } }),
  plug("manifest_hooks_null", {}, { manifest: { hooks: null } }),
  plug("manifest_top_level_null", {}, { manifest: "null" }),
  plug("manifest_top_level_array", {}, { manifest: "[1,2]" }),
  plug("manifest_top_level_string", {}, { manifest: '"x"' }),
  plug("manifest_not_json", {}, { manifest: "{" }),
  plug("manifest_bom", {}, { manifest: "\ufeff{\"hooks\":[]}" }),
  plug("manifest_ref_number", {}, { manifest: { hooks: [5] } }),
  plug("manifest_ref_null", {}, { manifest: { hooks: [null] } }),
  plug("manifest_ref_object", {}, { manifest: { hooks: [{}] } }),
  plug("manifest_second_ref_not_string", ev("Stop", [g([h()])]), { manifest: { hooks: ["./hooks/a.json", 7] } }),
  plug("doc_null", "null"),
  plug("doc_array", "[1]"),
  plug("doc_string", '"x"'),
  plug("doc_number", "7"),
  plug("doc_not_json", "{"),
  plug("doc_trailing_data", '{"hooks":{}} x'),
  plug("doc_nan", "NaN"),
  plug("doc_bom", "\ufeff{\"hooks\":{}}"),
  plug("doc_hooks_absent", {}),
  plug("doc_hooks_null", { hooks: null }),
  plug("doc_hooks_empty_object", { hooks: {} }),
  plug("doc_hooks_empty_array", { hooks: [] }),
  plug("doc_hooks_array", { hooks: [1] }),
  plug("doc_hooks_empty_string", { hooks: "" }),
  plug("doc_hooks_string", { hooks: "ab" }),
  plug("doc_hooks_number", { hooks: 5 }),
  plug("doc_hooks_true", { hooks: true }),
  plug("event_unknown", ev("Unknown", [g([h()])])),
  plug("event_lowercase", ev("stop", [g([h()])])),
  plug("event_unknown_after_valid", { hooks: { Stop: [g([h()])], Unknown: [] } }),
  plug("event_not_array_object", ev("Stop", {})),
  plug("event_not_array_string", ev("Stop", "x")),
  plug("event_not_array_null", ev("Stop", null)),
  plug("event_not_array_number", ev("Stop", 5)),
  plug("int_keys_first", '{"hooks":{"PreToolUse":5,"1":[],"0":[]}}'),
  plug("int_keys_numeric_order", '{"hooks":{"10":[],"2":[]}}'),
  plug("int_key_largest_index", '{"hooks":{"Stop":5,"4294967294":[],"4294967295":[]}}'),
  plug("int_key_not_an_index", '{"hooks":{"Stop":5,"4294967295":[],"01":[],"-1":[]}}'),
  ...protoEvents.map((e) => plug("proto_" + e, ev(e, [g([h()])]))),
  plug("proto_proto", '{"hooks":{"__proto__":[{"hooks":[{"type":"command","command":"echo ok"}]}]}}'),
  plug("proto_matcher_kept", ev("toString", [g([h()], "^x$")])),
  plug("proto_not_array", ev("toString", "x")),
  plug("proto_empty_array", ev("toString", [])),
  ...[null, 5, "x", true, false, 0, ""].map((v, i) => plug("group_invalid_" + i, ev("Stop", [v]))),
  plug("group_second_invalid", ev("Stop", [g([h()]), null])),
  plug("group_is_array", ev("Stop", [[]])),
  plug("group_empty_object", ev("Stop", [{}])),
  plug("group_hooks_object", ev("Stop", [{ hooks: {} }])),
  plug("group_hooks_string", ev("Stop", [{ hooks: "x" }])),
  plug("group_hooks_null", ev("Stop", [{ hooks: null }])),
  ...[5, null, {}, true, []].map((m, i) => plug("matcher_not_string_" + i, ev("Stop", [{ matcher: m, hooks: [h()] }]))),
  matcher("matcher_empty_kept", ""),
  matcher("matcher_star_kept", "*"),
  matcher("matcher_literal_kept", "^Bash$"),
  matcher("matcher_unicode_kept", "한글|Bash"),
  matcher("matcher_lookahead_kept", "^(?=Bash)"),
  matcher("matcher_negative_lookahead_kept", "^(?!Bash)"),
  matcher("matcher_lookbehind_kept", "(?<=a)b"),
  matcher("matcher_negative_lookbehind_kept", "(?<!a)b"),
  matcher("matcher_backreference_kept", "(a)\\1"),
  matcher("matcher_quantified_backreference_kept", "(a)\\1*"),
  matcher("matcher_named_backreference_kept", "(?<x>a)\\k<x>"),
  matcher("matcher_escaped_paren_then_unmatched_skipped", "\\(?=a)"),
  matcher("matcher_bracket_skipped", "["),
  matcher("matcher_paren_skipped", "("),
  matcher("matcher_repeat_skipped", "a**"),
  matcher("matcher_leading_repeat_skipped", "+a"),
  matcher("matcher_bad_range_skipped", "[z-a]"),
  matcher("matcher_bad_lookahead_skipped", "(?=a"),
  matcher("matcher_lookahead_and_syntax_error_skipped", "(?=a)["),
  plug("matcher_invalid_group_not_validated", ev("PreToolUse", [g([5, null], "[")])),
  plug("matcher_invalid_group_hooks_not_array", ev("PreToolUse", [{ matcher: "[", hooks: "x" }])),
  plug("matcher_invalid_group_skipped_next_kept", ev("PreToolUse", [g([h()], "["), g([h()], "^ok$")])),
  matcher("matcher_residual_inline_flag", "(?i)a"),
  matcher("matcher_residual_empty_class_negated", "[^]"),
  matcher("matcher_residual_unicode_escape", "\\u0041"),
  matcher("matcher_residual_quantified_anchor", "^*"),
  matcher("matcher_residual_repeat_limit", "a{1001}"),
  matcher("matcher_residual_named_group_python", "(?P<n>a)"),
  ...[null, 5, "x", true].map((v, i) => plug("handler_invalid_" + i, ev("Stop", [g([v])]))),
  plug("handler_second_invalid", ev("Stop", [g([h(), 5])])),
  plug("handler_is_array", ev("Stop", [g([[], h()])])),
  plug("type_prompt", stop({ type: "prompt", command: "x" })),
  plug("type_missing", stop({ command: "x" })),
  plug("type_null", stop({ type: null, command: "x" })),
  plug("type_number", stop({ type: 5, command: "x" })),
  plug("command_missing", stop({ type: "command" })),
  plug("command_null", stop({ type: "command", command: null })),
  plug("command_number", stop({ type: "command", command: 5 })),
  plug("command_empty", stop(h({ command: "" }))),
  plug("command_spaces", stop(h({ command: "   " }))),
  plug("command_tab_newline", stop(h({ command: "\t\n" }))),
  plug("command_nbsp", stop(h({ command: "\u00a0" }))),
  plug("command_bom_char", stop(h({ command: "\ufeff" }))),
  plug("command_ideographic_space", stop(h({ command: "\u3000" }))),
  plug("command_line_separator", stop(h({ command: "\u2028" }))),
  plug("command_next_line_kept", stop(h({ command: "\u0085" }))),
  plug("command_zero_width_space_kept", stop(h({ command: "\u200b" }))),
  plug("async_true_skipped", stop(h({ async: true }))),
  plug("async_false_kept", stop(h({ async: false }))),
  plug("async_null_kept", stop(h({ async: null }))),
  plug("async_one_refused", stop(h({ async: 1 }))),
  plug("async_string_refused", stop(h({ async: "true" }))),
  plug("timeout_zero_clamped", stop(h({ timeout: 0 }))),
  plug("timeout_string_refused", stop(h({ timeout: "5" }))),
  plug("timeout_huge", '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x","timeout":1e400}]}]}}'),
  plug("timeout_beyond_2p53", '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x","timeout":9007199254740993}]}]}}'),
  plug("status_message_kept", stop(h({ statusMessage: "Checking" }))),
  plug("status_message_number_refused", stop(h({ statusMessage: 5 }))),
  plug("handler_extra_fields_ignored", stop(h({ extra: { nested: [1, 2] }, timeout: 30 }))),
  plug("indices_with_skips", ev("PreToolUse", [g([h({ async: true }), h({ command: "one" }), { type: "prompt" }, h({ command: "three" })], "^a$"), g([h({ command: "g1" })])])),
  plug("command_multiline_unicode", stop(h({ command: "echo é日😀\nsecond" }))),
  plug("lone_surrogate_command", '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo \\ud800"}]}]}}'),
  plug("utf8_one_invalid_byte", withCommand([0xff])),
  plug("utf8_two_invalid_bytes", withCommand([0xff, 0xfe])),
  plug("utf8_truncated_sequence_then_ascii", withCommand([0xe2, 0x82, 0x41])),
  plug("utf8_truncated_four_byte_then_ascii", withCommand([0xf0, 0x9f, 0x41])),
  plug("utf8_encoded_surrogate_bytes", withCommand([0xed, 0xa0, 0x80])),
  plug("utf8_overlong_lead", withCommand([0xc0, 0x80])),
  plug("utf8_beyond_max_lead", withCommand([0xf5, 0x80, 0x80, 0x80])),
  plug("utf8_valid_four_byte", withCommand("😀")),
  plug("file_digest_of_raw_bytes", bytes('{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x\\u00e9"}]}]}}\n\n')),
];

const answerOf = (error) => {
  if (error.code) return { errorClass: error.code };
  if (error.name === "SyntaxError") return { errorClass: "SyntaxError" };
  return { error: error.message };
};

const recorded = [];
for (const c of cases) {
  const base = mkdtempSync(join(tmpdir(), "hte-"));
  const previous = process.cwd();
  try {
    for (const [rel, content] of Object.entries(c.files)) {
      mkdirSync(dirname(join(base, rel)), { recursive: true });
      writeFileSync(join(base, rel), typeof content === "string" ? content : Buffer.from(content.base64, "base64"));
    }
    for (const [rel, target] of Object.entries(c.links ?? {})) {
      mkdirSync(dirname(join(base, rel)), { recursive: true });
      symlinkSync(target, join(base, rel));
    }
    let pluginRoot = join(base, c.rootName ?? "plugin");
    if (c.relativeRoot) {
      process.chdir(base);
      pluginRoot = "plugin";
    }
    let answer;
    try {
      answer = { entries: listHookEntries(pluginRoot, c.key ?? KEY).map((e) => ({ key: e.key, hash: e.hash, fileSha256: e.fileSha256 })) };
    } catch (error) {
      answer = answerOf(error);
    }
    const record = { name: c.name, key: c.key ?? KEY };
    if (c.rootName) record.rootName = c.rootName;
    if (c.relativeRoot) record.relativeRoot = true;
    recorded.push({ ...record, files: c.files, ...(c.links ? { links: c.links } : {}), ...answer });
  } finally {
    process.chdir(previous);
    rmSync(base, { recursive: true, force: true });
  }
}

const names = new Set();
for (const c of recorded) {
  if (names.has(c.name)) throw new Error("duplicate case " + c.name);
  names.add(c.name);
}
writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/hook-trust.js",
  node: process.version,
  note: "Each case is a tree under one base directory (files by relative path: text, or {base64} for bytes; links: symlink targets) and the oracle's answer: entries, error (structured message) or errorClass (engine error).",
  cases: recorded,
}, null, 1) + "\n");
const entries = recorded.reduce((n, c) => n + (c.entries?.length ?? 0), 0);
console.log("recorded " + recorded.length + " cases (" + entries + " entries) to " + out);
