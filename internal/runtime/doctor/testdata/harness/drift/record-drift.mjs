// Records what CXC v0.2.40's doctor drift and ast-grep checks answer over the cases below; the
// Go test (harness_drift_test.go) replays testdata/harness/drift/oracle.json, so no Node is
// needed at test time. Recorded with Node v24.20.0 (the corpus's) as
//   node record-drift.mjs <oracle cxc-ops dist dir> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d, plugins/codexclaw/components/cxc-ops/dist).
//
// The run gets temporary HOME, CODEX_HOME and CRW_HOME directories and a temporary plugin root;
// it writes nothing outside them.
//
// Engine errors: the evidence of a check the oracle built from a swallowed readFileSync or
// JSON.parse error carries a host path (ENOENT, EISDIR) or a V8 message (SyntaxError), so such a
// check records its error CLASS beside a prefix (the class form testdata/hooktrust/
// record-entries.mjs uses) and the Go replay asserts the Go error of that class, never the text.
// A swallowed V8 TypeError names no host and stays exact: the Go port answers that same string.
//
// The files of a case are recorded in the oracle's names, exactly as the corpus fixtures hold
// them (skills/ast-grep/...); the Go test builds them through the names decision
// (contract/schema/cxc/name-substitution.json: the folder cxc-ast-grep is renamed crw-ast-grep),
// so the helper call the port makes is checked as skills/crw-ast-grep/scripts/.
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";

const [distArg, outArg] = process.argv.slice(2);
if (!distArg || !outArg) throw new Error("usage: node record-drift.mjs <oracle cxc-ops dist dir> <out file>");
const dist = resolve(distArg);
const out = resolve(outArg);

const temp = mkdtempSync(join(tmpdir(), "cxc-drift-record-"));
process.env.HOME = temp;
process.env.CODEX_HOME = join(temp, "codex");
process.env.CRW_HOME = join(temp, "crw");
process.chdir(temp);

const { runAstGrepCheck, runDriftCheck } = await import(resolve(dist, "doctor.js"));

const check = ({ name, severity, evidence, repair }) => {
  const result = { name, severity, evidence };
  if (repair !== undefined) result.repair = repair;
  return result;
};

// errorClass is the class of the engine error a check swallowed into its evidence: readFileSync
// answers ENOENT, EISDIR or EACCES through error.code, JSON.parse throws a SyntaxError, and a
// property read of a JSON null throws the V8 TypeError.
const classOf = (error) => {
  if (error.code) return error.code;
  if (error.name === "SyntaxError") return "SyntaxError";
  if (error.name === "TypeError") return "TypeError";
  return "unknown";
};

// The probes mirror the oracle's own reads (doctor.ts:549-575) so the class recorded beside a
// check is the class of the error that check swallowed.
const probeManifest = (plugin) => {
  try {
    const manifest = JSON.parse(readFileSync(join(plugin, ".codex-plugin", "plugin.json"), "utf8"));
    if (manifest === null) throw new TypeError("Cannot read properties of null (reading 'version')");
    return null;
  } catch (error) {
    return { errorClass: classOf(error) };
  }
};

const probeMCP = (plugin, manifest) => {
  if (manifest === null || typeof manifest !== "object" || typeof manifest.mcpServers !== "string" || manifest.mcpServers === "") return null;
  try {
    const mcp = JSON.parse(readFileSync(join(plugin, manifest.mcpServers), "utf8"));
    if (mcp === null) throw new TypeError("Cannot read properties of null (reading 'mcpServers')");
    return null;
  } catch (error) {
    return { errorClass: classOf(error) };
  }
};

const writeTree = (root, files, dirs) => {
  for (const rel of dirs ?? []) mkdirSync(join(root, rel), { recursive: true });
  for (const [rel, content] of Object.entries(files ?? {})) {
    const path = join(root, rel);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
};

// runDriftCheck (doctor.ts:543-596): drift:version, drift:mcp and the known-issues hint, in that
// order, from one plugin root. A manifest that cannot be read, parsed or member-read leaves a
// two-entry list: drift:version FAIL and known-issues.
const driftCases = [
  {
    name: "version_and_mcp_ok",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ name: "crw", version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ mcpServers: { one: { command: "x" }, two: { command: "y" } } }),
    },
  },
  { name: "version_missing", files: { ".codex-plugin/plugin.json": JSON.stringify({ name: "crw" }) } },
  { name: "version_non_string", files: { ".codex-plugin/plugin.json": JSON.stringify({ version: 7 }) } },
  { name: "manifest_number", files: { ".codex-plugin/plugin.json": "5" } },
  { name: "version_empty_string", files: { ".codex-plugin/plugin.json": JSON.stringify({ version: "" }) } },
  { name: "manifest_null", files: { ".codex-plugin/plugin.json": "null" } },
  { name: "manifest_missing", files: {} },
  { name: "manifest_unparseable", files: { ".codex-plugin/plugin.json": "{ not json" } },
  { name: "manifest_is_directory", dirs: [".codex-plugin/plugin.json"], files: {} },
  {
    name: "mcp_missing_file",
    files: { ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./gone.json" }) },
  },
  {
    name: "mcp_unparseable",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": "not json",
    },
  },
  {
    name: "mcp_null",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": "null",
    },
  },
  {
    name: "mcp_ref_is_directory",
    dirs: [".mcp.json"],
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
    },
  },
  {
    name: "mcp_zero_servers",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ other: 1 }),
    },
  },
  {
    name: "mcp_servers_array",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ mcpServers: [1, 2, 3] }),
    },
  },
  {
    name: "mcp_servers_string",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ mcpServers: "abc" }),
    },
  },
  {
    name: "mcp_servers_astral_string",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ mcpServers: "a\u{1F600}" }),
    },
  },
  {
    name: "mcp_primitive",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": "5",
    },
  },
  {
    name: "mcp_servers_number",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ mcpServers: 5 }),
    },
  },
  {
    name: "mcp_servers_null",
    files: {
      ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "./.mcp.json" }),
      ".mcp.json": JSON.stringify({ mcpServers: null }),
    },
  },
  { name: "mcp_no_reference", files: { ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0" }) } },
  { name: "mcp_empty_reference", files: { ".codex-plugin/plugin.json": JSON.stringify({ version: "0.4.0", mcpServers: "" }) } },
];

const driftRecorded = driftCases.map((testCase) => {
  const root = mkdtempSync(join(temp, "case-"));
  const plugin = join(root, "plugin");
  mkdirSync(plugin, { recursive: true });
  writeTree(plugin, testCase.files, testCase.dirs);
  const manifestProbe = probeManifest(plugin);
  let manifest = null;
  try {
    manifest = JSON.parse(readFileSync(join(plugin, ".codex-plugin", "plugin.json"), "utf8"));
  } catch {
    manifest = null;
  }
  const mcpProbe = probeMCP(plugin, manifest);
  const checks = runDriftCheck(plugin).map((c) => {
    const entry = check(c);
    entry.evidence = entry.evidence.replaceAll(root, "{ROOT}");
    const manifestPrefix = "cannot read manifest for drift baseline: ";
    const mcpPrefix = `${manifest?.mcpServers ?? ""} is unparseable: `;
    if (entry.evidence.startsWith(manifestPrefix) && manifestProbe) {
      entry.evidencePrefix = manifestPrefix;
      entry.errorClass = manifestProbe.errorClass;
    } else if (entry.evidence.startsWith(mcpPrefix) && mcpProbe) {
      entry.evidencePrefix = mcpPrefix;
      entry.errorClass = mcpProbe.errorClass;
    }
    return entry;
  });
  return {
    name: testCase.name,
    files: testCase.files ?? {},
    dirs: testCase.dirs ?? [],
    checks,
  };
});

// runAstGrepCheck (doctor.ts:597-645), POSIX branch: the helper under skills/ast-grep, python3
// with the oracle 8 s timeout, 127 or spawnSync ENOENT is a missing interpreter, a version match
// after a zero exit is PASS, anything else is the install hint.
const helperRelative = "skills/ast-grep/scripts/ast_grep_helper.py";

const astGrepCases = [
  { name: "helper_missing", files: {} },
  { name: "interpreter_missing_127", run: { status: 127, stdout: "", stderr: "" }, files: { [helperRelative]: "# stub\n" } },
  { name: "interpreter_missing_enoent", run: { status: null, error: "ENOENT", stdout: "", stderr: "" }, files: { [helperRelative]: "# stub\n" } },
  {
    name: "helper_success",
    run: { status: 0, stdout: "ast-grep binary: /opt/homebrew/bin/ast-grep\n  version: ast-grep 0.44.0\n", stderr: "" },
    files: { [helperRelative]: "# stub\n" },
  },
  {
    name: "helper_success_without_path",
    run: { status: 0, stdout: "  version: ast-grep 0.44.0\n", stderr: "" },
    files: { [helperRelative]: "# stub\n" },
  },
  {
    name: "version_in_stderr",
    run: { status: 0, stdout: "", stderr: "\n  version: ast-grep 1.2.3\n" },
    files: { [helperRelative]: "# stub\n" },
  },
  { name: "status0_without_version", run: { status: 0, stdout: "ast-grep binary: /opt/sg\n", stderr: "" }, files: { [helperRelative]: "# stub\n" } },
  { name: "sg_unresolved_exit_1", run: { status: 1, stdout: "ast-grep binary: NOT FOUND\n", stderr: "" }, files: { [helperRelative]: "# stub\n" } },
  { name: "timeout_killed", run: { status: null, error: "ETIMEDOUT", signal: "SIGTERM", stdout: "", stderr: "" }, files: { [helperRelative]: "# stub\n" } },
  {
    name: "nbsp_separators",
    run: { status: 0, stdout: "ast-grep\u00a0binary:\u00a0/opt/sg\n  version: ast-grep\u00a00.44.1\n", stderr: "" },
    files: { [helperRelative]: "# stub\n" },
  },
  {
    name: "nel_is_not_javascript_whitespace",
    run: { status: 0, stdout: "ast-grep\u00850.44.1", stderr: "" },
    files: { [helperRelative]: "# stub\n" },
  },
];

const astGrepRecorded = astGrepCases.map((testCase) => {
  const root = mkdtempSync(join(temp, "case-"));
  const plugin = join(root, "plugin");
  mkdirSync(plugin, { recursive: true });
  writeTree(plugin, testCase.files);
  const call = { made: 0 };
  const runner = (file, args, options) => {
    call.made += 1;
    call.file = file;
    call.args = args.map((arg) => arg.replaceAll(root, "{ROOT}"));
    call.timeout = options?.timeout ?? null;
    const shape = testCase.run ?? { status: 0, stdout: "", stderr: "" };
    return {
      status: shape.status ?? null,
      signal: shape.signal ?? null,
      error: shape.error ? { code: shape.error, message: `spawnSync ${file} ${shape.error}` } : undefined,
      stdout: shape.stdout ?? "",
      stderr: shape.stderr ?? "",
    };
  };
  return {
    name: testCase.name,
    files: testCase.files,
    run: testCase.run ?? null,
    check: check(runAstGrepCheck(plugin, runner, "linux")),
    call: call.made ? { file: call.file, args: call.args, timeout: call.timeout } : null,
  };
});

writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/doctor.js",
  node: process.version,
  note: "Each drift case holds the given files (oracle names) and the oracle checks. A check whose evidence the oracle built from a swallowed engine error carries evidencePrefix + errorClass instead of a comparable text when that error names a host path (ENOENT, EISDIR) or is a V8 parse error (SyntaxError); a swallowed V8 TypeError names no host and stays exact. Each ast-grep case holds the given files, the runner shape (status/error/signal) and the oracle answer; the Go replay maps status null + error ENOENT to a run with no status and status null + error ETIMEDOUT to a killed run (Go exit code -1), the two readings the oracle tells apart by error.code and signal. Files keep the oracle spelling so the Go test applies the names decision (skills/ast-grep -> skills/crw-ast-grep).",
  drift: driftRecorded,
  astGrep: astGrepRecorded,
}, null, 2) + "\n");
console.log(`recorded ${driftRecorded.length} drift and ${astGrepRecorded.length} ast-grep cases to ${out}`);
