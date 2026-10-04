// CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d), pabcd-state/src/divergence-cli.ts:18-165.
// Records the answers of runDivergenceCli for the Go port (CRW-546); the Go test replays them from
// oracle.json (no Node at test time). Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist <work-root> <out-dir>
// where <oracle> is a read-only extraction of CXC v0.2.40 (package version 0.2.40): its
// src/divergence-cli.ts is sha256
// f46379bed2225ca006779405d1ce4db4e31eca8333aa5ba1b08e8a7f33970b64 and the built
// dist/divergence-cli.js this module imports is sha256
// b5f1033b1adaded1fb557626f9e6efd651af3d9805ea022230f0c877f5d44e52.
// Only the output directory is written; the oracle is imported read-only.
//
// A case is {id, cwds: 1|2, setup?: {<cwd0-relative path>: text}, steps: [{argv}]}: the case gets
// one or two fresh mkdtemp cwds, runs its steps in order with the process cwd set to cwd0 (so a
// "--cwd <CWD1>" or a "--cwd \"\"" case resolves as it does for a user), and records each step's
// {code, output} (a throw is {threw: true}). <CWD0>/<CWD1> in argv and setup paths are replaced with
// the case's directories; the recorded argv keeps the placeholders. ISO timestamps in outputs and
// files are replaced with <TS>. Files are recorded relative to the cwd's .codexclaw directory, and
// .codexclaw/.gitignore is not recorded (its text is renamed by the port and carries no behaviour).
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

const [dist, workRoot, out] = process.argv.slice(2);
if (!dist || !workRoot || !out) {
  console.error("usage: node record-oracle.mjs <oracle-dist-dir> <work-root> <out-dir>");
  process.exit(2);
}
const workRootAbs = resolve(workRoot);
const outFile = join(resolve(out), "oracle.json");
const { runDivergenceCli } = await import(dist + "/divergence-cli.js");
const TS = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z/g;
const norm = (s) => s.replace(TS, "<TS>");
const ascii = (s) => s.replace(/[\u007f-\uffff]/g, (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));

// The files under <cwd>/.codexclaw, by path relative to it; .gitignore is skipped. Absent is {}.
function tree(cwd) {
  const walk = (dir, rel) => {
    let entries = [];
    try {
      entries = readdirSync(dir, { withFileTypes: true });
    } catch {
      return {};
    }
    return entries.reduce((out, e) => {
      if (e.name === ".gitignore") return out;
      const path = rel ? rel + "/" + e.name : e.name;
      if (e.isDirectory()) return { ...out, ...walk(join(dir, e.name), path) };
      let text = "";
      try {
        text = readFileSync(join(dir, e.name), "utf8");
      } catch {
        return out;
      }
      return { ...out, [path]: norm(text) };
    }, {});
  };
  return walk(join(cwd, ".codexclaw"), "");
}

const cases = [];
const add = (id, steps, extra = {}) => cases.push({ id, cwds: extra.cwds ?? 1, ...(extra.setup ? { setup: extra.setup } : {}), steps: steps.map((argv) => ({ argv })) });
const S = ["--session", "cli"];
const ADD = ["candidate", "add", ...S, "--kind", "strong-1", "--title", "T", "--rationale", "r"];

add("no_arguments", [[]]);
add("help_word", [["help"]]);
add("help_dashdash", [["--help"]]);
add("help_dash_h", [["-h"]]);
add("help_ignores_rest", [["--help", "--session", "s"]]);
add("help_topic_wins", [["help", "mode", "--session", "s"]]);
add("missing_session", [["mode", "on"]]);
add("short_session_unset", [["mode", "-s", "cli"]]);
add("session_empty", [["mode", "--session", ""]]);
add("session_consumes_flag", [["mode", "on", "--session", "--json"]]);
add("session_first_wins", [["mode", ...S, "--session", "other"]]);
add("session_beats_short", [["mode", "-s", "other", ...S]]);
add("unknown_topic_usage", [["bogus", ...S]]);
add("unknown_topic_no_session", [["bogus"]]);
add("unknown_verb_usage", [["candidate", "bogus", ...S]]);
add("mode_read_unset", [["mode", ...S]]);
add("mode_read_unset_json", [["mode", ...S, "--json"]]);
add("mode_read_unknown_verb", [["mode", "bogus", ...S]]);
add("mode_on_text", [["mode", "on", ...S]]);
add("mode_on_json", [["mode", "on", ...S, "--json"]]);
add("mode_on_collapse_reason", [["mode", "on", ...S, "--collapse", "P", "--reason", "why", "--json"]]);
add("mode_on_invalid_collapse", [["mode", "on", ...S, "--collapse", "X", "--json"]]);
add("mode_on_empty_reason", [["mode", "on", ...S, "--reason", "", "--json"]]);
add("mode_off_json", [["mode", "off", ...S, "--json"]]);
add("mode_on_then_off", [["mode", "on", ...S, "--json"], ["mode", "off", ...S, "--json"]]);
add("mode_write_read_roundtrip", [["mode", "on", ...S, "--collapse", "P", "--reason", "flat", "--json"], ["mode", ...S, "--json"], ["mode", ...S]]);
add("mode_trailing_cwd_ignored", [["mode", "on", ...S, "--cwd"]]);
add("mode_second_cwd", [["mode", "on", ...S, "--cwd", "<CWD1>", "--json"]], { cwds: 2 });
add("mode_read_other_cwd_unset", [["mode", ...S, "--cwd", "<CWD1>"]], { cwds: 2 });
add("mode_empty_cwd_writes_relative", [["mode", "on", ...S, "--cwd", "", "--json"]]);
add("mode_write_blocked_by_file", [["mode", "on", ...S]], { setup: { ".codexclaw/divergence": "not a directory\n" } });
add("mode_write_cwd_is_a_file", [["mode", "on", ...S, "--cwd", "<CWD0>/blocker"]], { setup: { "blocker": "x\n" } });
add("candidate_add_missing_kind", [["candidate", "add", ...S, "--title", "T", "--rationale", "r", "--source", "https://example.invalid/a"]]);
add("candidate_add_invalid_kind", [[...ADD, "--kind", "bogus", "--source", "https://example.invalid/a"]]);
add("candidate_add_text", [[...ADD, "--source", "https://example.invalid/a"]]);
add("candidate_add_full_json", [[...ADD, "--source", "https://example.invalid/a", "--status", "built", "--change-class", "state-space-redesign", "--killed-at-phase", "D", "--worktree", "/wt", "--json"]]);
add("candidate_add_status_invalid", [[...ADD, "--status", "bogus", "--source", "https://example.invalid/a", "--json"]]);
add("candidate_add_change_class_empty", [[...ADD, "--change-class", "", "--source", "https://example.invalid/a", "--json"]]);
add("candidate_add_change_class_invalid", [[...ADD, "--change-class", "bogus", "--source", "https://example.invalid/a"]]);
add("candidate_add_killed_at_empty", [[...ADD, "--killed-at-phase", "", "--source", "https://example.invalid/a", "--json"]]);
add("candidate_add_killed_at_invalid", [[...ADD, "--killed-at-phase", "X", "--source", "https://example.invalid/a"]]);
add("candidate_add_missing_title", [["candidate", "add", ...S, "--kind", "strong-1", "--rationale", "r", "--source", "https://example.invalid/a"]]);
add("candidate_add_missing_rationale", [["candidate", "add", ...S, "--kind", "strong-1", "--title", "T", "--source", "https://example.invalid/a"]]);
add("candidate_add_missing_source", [ADD]);
add("candidate_add_source_empty", [[...ADD, "--source", ""]]);
add("candidate_add_source_whitespace", [[...ADD, "--source", " "]]);
add("candidate_add_source_trailing", [[...ADD, "--source"]]);
add("candidate_add_source_repeated", [[...ADD, "--source", "https://example.invalid/a", "--source", "https://example.invalid/a", "--source", "https://example.invalid/b", "--json"]]);
add("candidate_list_empty", [["candidate", "list", ...S]]);
add("candidate_list_empty_json", [["candidate", "list", ...S, "--json"]]);
add("candidate_list_nonempty_text", [[...ADD, "--source", "https://example.invalid/a"], ["candidate", "list", ...S]]);
add("candidate_list_nonempty_json", [[...ADD, "--source", "https://example.invalid/a", "--json"], ["candidate", "list", ...S, "--json"]]);
add("candidate_list_other_session", [[...ADD, "--source", "https://example.invalid/a"], ["candidate", "list", ...S, "--session", "other"]]);
add("candidate_add_second_cwd", [[...ADD, "--cwd", "<CWD1>", "--source", "https://example.invalid/a"], ["candidate", "list", ...S], ["candidate", "list", ...S, "--cwd", "<CWD1>"]], { cwds: 2 });

mkdirSync(workRootAbs, { recursive: true });
const recorded = [];
for (const c of cases) {
  const cwd0 = mkdtempSync(join(workRootAbs, "case-"));
  const cwd1 = c.cwds === 2 ? mkdtempSync(join(workRootAbs, "case-")) : null;
  const sub = (s) => String(s).replaceAll("<CWD0>", cwd0).replaceAll("<CWD1>", cwd1 ?? cwd0);
  for (const [rel, text] of Object.entries(c.setup ?? {})) {
    const path = join(cwd0, sub(rel));
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, text);
  }
  process.chdir(cwd0);
  const steps = c.steps.map(({ argv }) => {
    try {
      const result = runDivergenceCli(argv.map(sub), cwd0);
      return { code: result.code, output: norm(result.output) };
    } catch {
      return { threw: true };
    }
  });
  recorded.push({ id: c.id, cwds: c.cwds, ...(c.setup ? { setup: c.setup } : {}), steps: c.steps, want: { steps, files: { cwd0: tree(cwd0), cwd1: cwd1 ? tree(cwd1) : null } } });
  rmSync(cwd0, { recursive: true, force: true });
  if (cwd1) rmSync(cwd1, { recursive: true, force: true });
}
const body = JSON.stringify({ oracle: "CXC v0.2.40 3c1459acadeb1906d97c00a598e1457327ae372d", cases: recorded }, null, 2) + "\n";
writeFileSync(outFile, ascii(body));
console.log("recorded " + recorded.length + " divergence CLI cases");
