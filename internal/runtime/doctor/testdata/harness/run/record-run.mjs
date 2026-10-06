// Records what CXC v0.2.40's runDoctor answers over the cases below; the Go test
// (harness_run_test.go) replays testdata/harness/run/oracle.json, so no Node is needed at test
// time. Recorded with Node v24.20.0 as
//   node record-run.mjs <oracle cxc-ops dist dir> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d, plugins/codexclaw/components/cxc-ops/dist).
//
// runDoctor is reached directly (it is exported) with a stub runner, the same caller
// cxc-ops.test.ts uses. The recorded group is the assembly the port must reproduce: the check
// order, the three inline checks (manifest, skills, agents), the report metadata and the text
// render. Every recorded string is passed through toWellFormed() (what the UTF-8 encoder does to
// a lone surrogate) and the recorder's temporary root is replaced by the TEMP token.
//
// The run gets temporary HOME, CODEX_HOME and CRW_HOME directories and a cwd without .codexclaw;
// it writes nothing outside them.
import { mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [distArg, outArg] = process.argv.slice(2);
if (!distArg || !outArg) throw new Error("usage: node record-run.mjs <oracle cxc-ops dist dir> <out file>");
const dist = resolve(distArg);
const out = resolve(outArg);

const temp = mkdtempSync(join(tmpdir(), "cxc-doctor-run-"));
process.env.HOME = temp;
process.env.CODEX_HOME = join(temp, "codex");
process.env.CRW_HOME = join(temp, "crw");
process.env.CODEX_THREAD_ID = "rec-s1";
process.chdir(temp);

const { runDoctor, renderDoctor } = await import(resolve(dist, "doctor.js"));

const TEMP_TOKEN = String.fromCharCode(36) + "{TEMP}";
const token = (text) => (text == null ? null : String(text).split(temp).join(TEMP_TOKEN).toWellFormed());
const listing = (states) => ["multi_agent", "goals", "hooks", "default_mode_request_user_input"]
  .map((key) => key + "  stable  " + (states[key] === true)).join("\n");
const allOn = { multi_agent: true, goals: true, hooks: true, default_mode_request_user_input: true };
const offWsl = { platform: "linux", env: {}, procVersion: null };
const codexStub = (states, version) => (cmd, args) => {
  if (cmd === "codex" && Array.isArray(args) && args[0] === "features") return { status: 0, stdout: listing(states), stderr: "" };
  if (cmd === "codex" && Array.isArray(args) && args[0] === "--version") return { status: 0, stdout: version, stderr: "" };
  if (cmd === "python3") return { status: 0, stdout: "ast-grep binary: /stub/sg\n  version: ast-grep 0.44.0\n", stderr: "" };
  return { status: 1, stdout: "", stderr: "" };
};

// A payload builder mirroring cxc-ops.test.ts makePluginRoot, with knobs for the cases below.
function payload(name, opts) {
  opts = opts || {};
  const root = join(temp, "payload-" + name);
  mkdirSync(join(root, ".codex-plugin"), { recursive: true });
  if (opts.manifest !== "missing") {
    writeFileSync(join(root, ".codex-plugin", "plugin.json"), opts.manifest || JSON.stringify({ name: "crw", version: "0.0.1", hooks: ["./hooks/a.json"], mcpServers: "./.mcp.json" }));
  }
  if (opts.mcp !== false) writeFileSync(join(root, ".mcp.json"), JSON.stringify({ mcpServers: { test: { command: "node" } } }));
  if (opts.hooks !== false) {
    mkdirSync(join(root, "hooks"), { recursive: true });
    writeFileSync(join(root, "hooks", "a.json"), JSON.stringify({ hooks: { Stop: [{ hooks: [{ type: "command", command: "node stub.js" }] }] } }));
  }
  if (opts.skills !== "absent") {
    const names = opts.skills || ["dev", "ast-grep"];
    for (const skill of names) {
      mkdirSync(join(root, "skills", skill, "agents"), { recursive: true });
      writeFileSync(join(root, "skills", skill, "SKILL.md"), "---\nname: x\n---\n");
      if (!(opts.broken || []).includes(skill)) writeFileSync(join(root, "skills", skill, "agents", "openai.yaml"), "policy: {}\n");
    }
    // The ast-grep helper skill, complete even when it is not in names, so only the named
    // broken skills fail.
    if (!names.includes("ast-grep")) {
      mkdirSync(join(root, "skills", "ast-grep", "agents"), { recursive: true });
      writeFileSync(join(root, "skills", "ast-grep", "SKILL.md"), "---\nname: ast-grep\n---\n");
      writeFileSync(join(root, "skills", "ast-grep", "agents", "openai.yaml"), "policy: {}\n");
    }
    mkdirSync(join(root, "skills", "ast-grep", "scripts"), { recursive: true });
    writeFileSync(join(root, "skills", "ast-grep", "scripts", "ast_grep_helper.py"), "# stub\n");
  }
  if (opts.agents !== "absent") {
    mkdirSync(join(root, "agents"), { recursive: true });
    const roles = opts.roles || ["explorer"];
    for (const role of roles) writeFileSync(join(root, "agents", role + ".toml"), "name=\"" + role + "\"\n");
  }
  return root;
}

const codexHome = join(temp, "codex");
mkdirSync(codexHome, { recursive: true });

function record(name, root, opts) {
  opts = opts || {};
  for (const key of ["CODEX_SURFACE", "CODEX_APP_PORT"]) delete process.env[key];
  Object.assign(process.env, opts.env || {});
  const report = runDoctor(root, codexStub(opts.states || allOn, opts.version || "codex-cli 1.2.3\n"), { codexHome: codexHome, pluginKey: opts.pluginKey, sessionId: "rec-s1", wslDeps: offWsl });
  const recorded = {
    name: name,
    checks: report.checks.map((c) => ({ name: c.name, severity: c.severity, evidence: token(c.evidence), repair: token(c.repair) })),
    overall: report.overall,
    pluginVersion: report.pluginVersion == null ? null : token(report.pluginVersion),
    codexVersion: report.codexVersion == null ? null : token(report.codexVersion),
    activeSurface: report.activeSurface == null ? null : token(report.activeSurface),
    text: token(renderDoctor(report)),
  };
  for (const key of ["CODEX_SURFACE", "CODEX_APP_PORT"]) delete process.env[key];
  return recorded;
}

const cases = [];
cases.push(record("healthy_assembly", payload("healthy")));
cases.push(record("manifest_missing", payload("missing", { manifest: "missing" })));
cases.push(record("manifest_unparseable", payload("unparseable", { manifest: "{not json" })));
cases.push(record("manifest_hooks_not_an_array", payload("hooks-not-array", { manifest: JSON.stringify({ name: "crw", version: "0.0.1", hooks: "nope" }) })));
cases.push(record("skills_absent", payload("skills-absent", { skills: "absent" })));
cases.push(record("skills_broken", payload("skills-broken", { skills: ["dev", "broken"], broken: ["broken"] })));
cases.push(record("agents_absent", payload("agents-absent", { agents: "absent" })));
cases.push(record("no_plugin_version", payload("no-version", { manifest: JSON.stringify({ name: "crw", hooks: ["./hooks/a.json"] }) })));
cases.push(record("hard_flag_off_fails", payload("hard-off"), { states: Object.assign({}, allOn, { goals: false }) }));
cases.push(record("surface_env", payload("surface"), { env: { CODEX_SURFACE: "cli" } }));
cases.push(record("surface_app_port", payload("app-port"), { env: { CODEX_APP_PORT: "4500" } }));
cases.push(record("surface_empty_port_is_unset", payload("empty-port"), { env: { CODEX_APP_PORT: "" } }));
cases.push(record("codex_version_fallback", payload("version-fallback"), { version: "  nightly  \n" }));

writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/doctor.js",
  node: process.version,
  note: "runDoctor(root, stubRunner, {codexHome, pluginKey, wslDeps}) over a constructed payload. The stub runner answers 'codex features list', 'codex --version' and 'python3 <helper> doctor'. Each recorded string is toWellFormed() and the recorder's temporary root is the TEMP token. The check order here is the oracle's emission order, which the recorded corpus also prints.",
  cases: cases,
}, null, 2) + "\n");
rmSync(temp, { recursive: true, force: true });
console.log("recorded " + cases.length + " runDoctor cases to " + out);
