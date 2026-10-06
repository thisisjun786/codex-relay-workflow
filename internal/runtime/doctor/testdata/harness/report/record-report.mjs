// Records what CXC v0.2.40's doctor report core answers over the cases below; the Go test
// (harness_report_test.go) replays testdata/harness/report/oracle.json, so no Node is needed at
// test time. Recorded with Node v24.20.0 as
//   node record-report.mjs <oracle cxc-ops dist dir> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d, plugins/codexclaw/components/cxc-ops/dist).
//
// detectCodexVersion is not exported by doctor.ts, so its answers are recorded through the
// codexVersion field of runDoctor(root, runner, {codexHome, wslDeps}) with a stub runner: the
// private function is still the one answering, reached through the caller the oracle's own
// tests use (cxc-ops.test.ts calls runDoctor to pin `codex --version` parsing).
//
// The run gets temporary HOME, CODEX_HOME and CRW_HOME directories, a temporary plugin root and
// a cwd without .codexclaw; it writes nothing outside them.
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [distArg, outArg] = process.argv.slice(2);
if (!distArg || !outArg) throw new Error("usage: node record-report.mjs <oracle cxc-ops dist dir> <out file>");
// Both are resolved before the chdir below: a relative path would otherwise be read from the
// temporary root.
const dist = resolve(distArg);
const out = resolve(outArg);

const temp = mkdtempSync(join(tmpdir(), "cxc-doctor-record-"));
process.env.HOME = temp;
process.env.CODEX_HOME = join(temp, "codex");
process.env.CRW_HOME = join(temp, "crw");
// runDoctor reads process.cwd() for the pabcd-state and wsl checks; keep it a directory that
// holds no .codexclaw and is never WSL, so the recorded answers do not depend on where the
// recorder was invoked.
process.chdir(temp);

const {
  DOCTOR_DECLARED_FEATURES,
  buildDeclaredFeaturesCheck,
  checkWslResidency,
  parseDoctorFeatures,
  renderDoctor,
  rollup,
  runDoctor,
} = await import(resolve(dist, "doctor.js"));

const pluginRoot = join(temp, "payload");
mkdirSync(join(pluginRoot, ".codex-plugin"), { recursive: true });
writeFileSync(join(pluginRoot, ".codex-plugin", "plugin.json"), JSON.stringify({ name: "crw", version: "0.4.0" }));

const check = (name, severity, evidence = `${name} evidence`, repair) => {
  const result = { name, severity, evidence };
  if (repair !== undefined) result.repair = repair;
  return result;
};
const listing = (states) => DOCTOR_DECLARED_FEATURES.map((key) => `${key}  stable  ${states[key] === true}`).join("\n");
const allOn = { multi_agent: true, goals: true, hooks: true, default_mode_request_user_input: true };
const offWsl = { platform: "linux", env: {}, procVersion: null };

// rollup (doctor.ts:73-78): FAIL > WARN > PASS, an empty list PASSes.
const rollupCases = [
  { name: "empty_is_pass", checks: [] },
  { name: "single_pass", checks: [check("a", "PASS")] },
  { name: "warn_beats_pass", checks: [check("a", "PASS"), check("b", "WARN")] },
  { name: "fail_beats_warn", checks: [check("a", "WARN"), check("b", "FAIL")] },
  { name: "fail_first", checks: [check("a", "FAIL"), check("b", "WARN"), check("c", "PASS")] },
  { name: "foreign_severity_is_not_fail", checks: [check("a", "SKIPPED"), check("b", "PASS")] },
].map((testCase) => ({ ...testCase, overall: rollup(testCase.checks) }));

// renderDoctor (doctor.ts:648-661): one line per check, the repair on its own indented line and
// only for a non-PASS check, the codex header above the crw header above the checks, and the
// overall line last.
const renderCases = [
  { name: "empty_report", report: { schemaVersion: 1, overall: "PASS", checks: [] } },
  {
    name: "headers_codex_then_plugin",
    report: { schemaVersion: 1, overall: "WARN", pluginVersion: "0.3.1", codexVersion: "1.2.3", checks: [check("features", "WARN", "1/4 enabled", "cxc enable")] },
  },
  {
    name: "repair_only_on_non_pass",
    report: {
      schemaVersion: 1,
      overall: "FAIL",
      checks: [check("a", "PASS", "fine", "must not print"), check("b", "FAIL", "broken", "fix b"), check("c", "WARN", "shaky", "fix c")],
    },
  },
  {
    name: "non_ascii_evidence",
    report: { schemaVersion: 1, overall: "PASS", checks: [check("skills", "PASS", "3 skill(s) \u00b7 \uc138\uc158 \u2713 \u2014 caf\u00e9 \u{1f600}")] },
  },
  {
    name: "multiline_evidence_and_repair",
    report: {
      schemaVersion: 1,
      overall: "WARN",
      checks: [check("manifest", "WARN", "line one\nline two", "run:\n  crw install")],
    },
  },
  { name: "plugin_version_only", report: { schemaVersion: 1, overall: "PASS", pluginVersion: "0.0.1", checks: [check("manifest", "PASS")] } },
].map((testCase) => ({ ...testCase, text: renderDoctor(testCase.report).toWellFormed() }));

// parseDoctorFeatures (doctor.ts:118-130): blank, unknown, single-field and unparseable lines are
// skipped; the first field must equal a declared flag; the last field decides; a later line wins.
const parseCases = [
  { name: "real_table", stdout: listing(allOn) },
  { name: "blank_and_whitespace_lines", stdout: "\n   \n\t\n" + listing(allOn) + "\n\n" },
  { name: "unknown_keys_ignored", stdout: "web_search  stable  true\nplugin_hooks  removed  false\ngoals  stable  true" },
  { name: "repeated_key_last_line_wins", stdout: "goals  stable  true\ngoals  experimental  false" },
  { name: "repeated_key_reversed", stdout: "hooks  stable  false\nhooks  stable  true" },
  { name: "single_field_line_skipped", stdout: "goals\ngoals  stable  true" },
  { name: "sibling_keys_do_not_satisfy", stdout: "multi_agent_v2  experimental  true\nmulti_agent  stable  false\nplugin_hooks  stable  true\nhooks  stable  false" },
  { name: "unparseable_last_token", stdout: "goals  stable  maybe" },
  { name: "last_field_is_not_the_state_field", stdout: "goals  stable  true  extra" },
  { name: "uppercase_state", stdout: "goals  stable  TRUE\nhooks  stable  False" },
  { name: "crlf_lines", stdout: "goals  stable  true\r\nhooks  stable  false\r\n" },
  { name: "tabs_and_runs_of_spaces", stdout: "goals\t\tstable\ttrue\nhooks      stable   false" },
  { name: "nbsp_separated", stdout: "goals\u00a0stable\u00a0true" },
  { name: "bom_prefixed_line", stdout: "\ufeffgoals  stable  true" },
  { name: "nel_is_not_javascript_whitespace", stdout: "goals\u0085stable\u0085true" },
].map((testCase) => ({ ...testCase, parsed: Object.fromEntries(parseDoctorFeatures(testCase.stdout)) }));

// buildDeclaredFeaturesCheck (doctor.ts:132-164): PASS at 4/4, FAIL for a hard flag, WARN for the
// soft flag only, and a WARN that names no exit code when the spawn threw (status null).
const featuresCases = [
  { name: "all_on", run: { status: 0, stdout: listing(allOn), stderr: "" } },
  { name: "soft_off", run: { status: 0, stdout: listing({ ...allOn, default_mode_request_user_input: false }), stderr: "" } },
  { name: "hard_off_goals", run: { status: 0, stdout: listing({ ...allOn, goals: false }), stderr: "" } },
  { name: "hard_and_soft_off", run: { status: 0, stdout: listing({ ...allOn, hooks: false, default_mode_request_user_input: false }), stderr: "" } },
  { name: "all_off_empty_stdout", run: { status: 0, stdout: "", stderr: "" } },
  { name: "unparseable_only", run: { status: 0, stdout: "goals  stable  maybe\n", stderr: "" } },
  { name: "exit_127_with_stderr", run: { status: 127, stdout: "", stderr: "command not found" } },
  { name: "exit_1_trimmed_stderr", run: { status: 1, stdout: "", stderr: "   boom   " } },
  { name: "stderr_sliced_to_160", run: { status: 3, stdout: "", stderr: "x".repeat(200) } },
  { name: "stderr_slice_cuts_a_surrogate_pair", run: { status: 3, stdout: "", stderr: "x".repeat(159) + "\u{1f600}" + "y" } },
  { name: "status_null_stderr", run: { status: null, stdout: "", stderr: "EPERM" } },
  { name: "nonzero_without_stderr", run: { status: 2, stdout: "", stderr: "" } },
].map((testCase) => {
  const result = buildDeclaredFeaturesCheck(testCase.run);
  // The check strings are recorded as the oracle's JSON strings, NOT through toWellFormed(): a
  // stderr cut inside a surrogate pair keeps the lone high surrogate in evidence, which
  // JSON.stringify writes as the \\ud83d escape. The render group below keeps toWellFormed()
  // because it records the bytes the UTF-8 encoder writes to a terminal, where that surrogate
  // is U+FFFD.
  return { ...testCase, severity: result.severity, evidence: result.evidence, repair: result.repair ?? null };
});

// detectCodexVersion (doctor.ts:89-98) through runDoctor's codexVersion field: the first
// `\d+.\d+.\d+` in stdout, else the trimmed stdout, and undefined for a non-zero or null status,
// empty stdout or a spawn that threw.
const codexVersionCases = [
  { name: "semver", status: 0, stdout: "codex-cli 1.2.3\n", stderr: "" },
  { name: "first_of_two_versions", status: 0, stdout: "codex-cli 1.2.3 (build 4.5.6)\n", stderr: "" },
  { name: "version_without_prefix", status: 0, stdout: "1.2.3\n", stderr: "" },
  { name: "fallback_trimmed", status: 0, stdout: "  nightly  \n", stderr: "" },
  { name: "fallback_js_trim_nbsp", status: 0, stdout: "\u00a0nightly\u00a0", stderr: "" },
  { name: "nonzero_status", status: 1, stdout: "codex-cli 1.2.3\n", stderr: "boom" },
  { name: "empty_stdout", status: 0, stdout: "", stderr: "" },
  { name: "whitespace_only_stdout", status: 0, stdout: "   ", stderr: "" },
  { name: "null_status", status: null, stdout: "", stderr: "killed by signal" },
  { name: "throws", throws: true },
].map((testCase) => {
  const runner = (cmd, args) => {
    if (cmd === "codex" && Array.isArray(args) && args[0] === "features") return { status: 0, stdout: listing(allOn), stderr: "" };
    if (cmd === "codex") {
      if (testCase.throws) throw new Error("codex could not be spawned");
      return { status: testCase.status, stdout: testCase.stdout ?? "", stderr: testCase.stderr ?? "" };
    }
    return { status: 1, stdout: "", stderr: "" };
  };
  const report = runDoctor(pluginRoot, runner, { codexHome: join(temp, "codex"), wslDeps: offWsl });
  return { ...testCase, version: report.codexVersion ?? null };
});

// checkWslResidency (doctor.ts:193-196) off WSL: the only branch this port keeps.
const wsl = (() => {
  const result = checkWslResidency("/home/u/proj", offWsl);
  return { severity: result.severity, evidence: result.evidence };
})();

writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/doctor.js",
  node: process.version,
  note: "Each group holds the oracle answer for the inputs beside it. The render group records toWellFormed() text: that is what the UTF-8 encoder does to a lone surrogate on the way to stdout. The featuresCheck group records the oracle's JSON strings instead, so the lone high surrogate a stderr cut keeps is held as the \\ud83d escape JSON.stringify writes (doctor.ts:137). codexVersion cases are runDoctor(pluginRoot, stubRunner, {codexHome, wslDeps}) calls whose codexVersion field is detectCodexVersion, which doctor.ts does not export; the stub runner answers `codex features list` with all four declared flags true and the case `codex --version` behaviour.",
  rollup: rollupCases,
  render: renderCases,
  featuresParsed: parseCases,
  featuresCheck: featuresCases,
  codexVersion: codexVersionCases,
  wsl,
}, null, 2) + "\n");
console.log(`recorded ${rollupCases.length} rollup, ${renderCases.length} render, ${parseCases.length} parse, ${featuresCases.length} features, ${codexVersionCases.length} codex-version cases to ${out}`);
