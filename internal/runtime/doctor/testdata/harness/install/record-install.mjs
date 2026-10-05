// Records what CXC v0.2.40's doctor install checks answer over the cases below; the Go test
// (harness_install_test.go) replays testdata/harness/install/oracle.json, so no Node is needed at
// test time. Recorded with Node v24.20.0 as
//   node record-install.mjs <oracle cxc-ops dist dir> <out file>
// where the dist dir belongs to a read-only CXC v0.2.40 tree (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d, plugins/codexclaw/components/cxc-ops/dist).
//
// checkPabcdHealth and manifestTargetChecks are not exported by doctor.ts, so their answers are
// recorded through runDoctor(pluginRoot, runner, options) with a stub runner, filtering the
// checks they own (pabcd-state; hooks, mcp-targets, manifest-targets). runInstalledRootCheck is
// exported and called directly. The pabcd check reads process.cwd(), so each case chdirs into its
// own workspace; a readdir failure there is uncaught in the oracle, and the throw message is
// recorded in place of a check.
//
// Every recorded string has the recorder's temp root replaced by ${TEMP}; the Go test substitutes
// its own. The run gets temporary HOME, CODEX_HOME and CRW_HOME directories, a temporary plugin
// root and cwd; it writes nothing outside them.
import { chmodSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [distArg, outArg] = process.argv.slice(2);
if (!distArg || !outArg) throw new Error("usage: node record-install.mjs <oracle cxc-ops dist dir> <out file>");
const dist = resolve(distArg);
const out = resolve(outArg);

const temp = mkdtempSync(join(tmpdir(), "cxc-doctor-install-"));
process.env.HOME = join(temp, "home");
process.env.CODEX_HOME = join(temp, "codex");
process.env.CRW_HOME = join(temp, "crw");
process.chdir(temp);

const { runDoctor, runInstalledRootCheck } = await import(resolve(dist, "doctor.js"));

const sub = (text) => String(text).split(temp).join("${TEMP}");
const stubRunner = () => ({ status: 1, stdout: "", stderr: "stub codex: not scripted" });

// ---- pabcd state (checkPabcdHealth through runDoctor) ----------------------

const pluginRoot = join(temp, "payload");
mkdirSync(join(pluginRoot, ".codex-plugin"), { recursive: true });
writeFileSync(join(pluginRoot, ".codex-plugin", "plugin.json"), JSON.stringify({ name: "crw", version: "0.4.0" }));
// A payload whose hooks/mcp files exist, so only the pabcd check under test changes.
writeFileSync(join(pluginRoot, "hooks.json"), JSON.stringify({ hooks: { Stop: [] } }));

function pabcdCase(name, setup) {
  // Deterministic layout: the Go replay rebuilds the same relative tree under its own temp root
  // and substitutes ${TEMP}, so every recorded path is reproducible.
  const ws = join(temp, "ws-" + name);
  mkdirSync(ws, { recursive: true });
  setup(join(ws, ".codexclaw"));
  process.chdir(ws);
  try {
    const report = runDoctor(pluginRoot, stubRunner, { codexHome: join(temp, "codex") });
    const check = report.checks.find((c) => c.name === "pabcd-state");
    return { name, severity: sub(check.severity), evidence: sub(check.evidence), repair: sub(check.repair ?? "") };
  } catch (error) {
    return { name, threw: sub(error instanceof Error ? error.message : String(error)) };
  }
}

const deep = "[".repeat(10001) + "]".repeat(10001);
const pabcd = [
  pabcdCase("absent", () => {}),
  pabcdCase("clean", (state) => {
    mkdirSync(join(state, "sessions"), { recursive: true });
    writeFileSync(join(state, "sessions", "rec-s1.json"), JSON.stringify({ phase: "P" }));
    writeFileSync(join(state, "sessions", "note.txt"), "not a session");
  }),
  pabcdCase("corrupt_one", (state) => {
    mkdirSync(join(state, "sessions"), { recursive: true });
    writeFileSync(join(state, "sessions", "bad.json"), "{oops}");
  }),
  pabcdCase("corrupt_two", (state) => {
    mkdirSync(join(state, "sessions"), { recursive: true });
    writeFileSync(join(state, "sessions", "a.json"), "]");
    writeFileSync(join(state, "sessions", "b.json"), "{");
  }),
  pabcdCase("entry_is_directory", (state) => {
    mkdirSync(join(state, "sessions", "dir.json"), { recursive: true });
  }),
  pabcdCase("deep_10001", (state) => {
    mkdirSync(join(state, "sessions"), { recursive: true });
    writeFileSync(join(state, "sessions", "deep.json"), deep);
  }),
  pabcdCase("invalid_session_name", (state) => {
    // One ill-formed byte in the entry name: readdirSync decodes it to U+FFFD, and the lookup of
    // that decoded name fails, so the file is corrupt.
    mkdirSync(join(state, "sessions"), { recursive: true });
    const name = Buffer.concat([Buffer.from([0x62, 0xff]), Buffer.from(".json")]);
    writeFileSync(Buffer.concat([Buffer.from(join(state, "sessions") + "/"), name]), "{oops}");
  }),
  pabcdCase("unreadable", (state) => {
    mkdirSync(join(state, "sessions"), { recursive: true });
    chmodSync(join(state, "sessions"), 0o000);
  }),
];

// ---- manifest targets (manifestTargetChecks through runDoctor) ------------

function manifestCase(name, build) {
  // runDoctor reads process.cwd() for the pabcd check; keep it the neutral temp root so a
  // previous case's workspace (possibly unreadable) cannot abort this one.
  process.chdir(temp);
  const root = join(temp, "payload-" + name);
  mkdirSync(root, { recursive: true });
  mkdirSync(join(root, ".codex-plugin"), { recursive: true });
  const spec = build(root);
  const manifest = spec.raw ?? JSON.stringify({ name: "crw", version: "0.4.0", ...spec });
  writeFileSync(join(root, ".codex-plugin", "plugin.json"), manifest);
  const report = runDoctor(root, stubRunner, { codexHome: join(temp, "codex") });
  const names = ["hooks", "mcp-targets", "manifest-targets"];
  const checks = report.checks
    .filter((c) => names.includes(c.name))
    .map((c) => ({ name: c.name, severity: c.severity, evidence: sub(c.evidence), repair: sub(c.repair ?? "") }));
  return { name, checks };
}

const hookDoc = (command) => JSON.stringify({ hooks: { Stop: [{ hooks: [{ type: "command", command }] }] } });
const mcpDoc = (arg) => JSON.stringify({ mcpServers: { bridge: { command: "node", args: [arg] } } });
const manifestTargets = [
  manifestCase("missing_hook_target", (root) => {
    writeFileSync(join(root, "hooks.json"), hookDoc('node "${PLUGIN_ROOT}/dist/missing.js"'));
    return { hooks: ["./hooks.json"] };
  }),
  manifestCase("missing_mcp_target", (root) => {
    writeFileSync(join(root, ".mcp.json"), mcpDoc("${PLUGIN_ROOT}/dist/missing.js"));
    return { mcpServers: "./.mcp.json" };
  }),
  manifestCase("empty_target", (root) => {
    mkdirSync(join(root, "dist"), { recursive: true });
    writeFileSync(join(root, "dist/empty.js"), "");
    writeFileSync(join(root, "hooks.json"), hookDoc('node "${PLUGIN_ROOT}/dist/empty.js"'));
    return { hooks: ["./hooks.json"] };
  }),
  manifestCase("escapes_root", (root) => {
    writeFileSync(join(root, "hooks.json"), hookDoc('node "${PLUGIN_ROOT}/../outside.js"'));
    return { hooks: ["./hooks.json"] };
  }),
  manifestCase("unparseable_hook", (root) => {
    writeFileSync(join(root, "hooks.json"), "not json");
    return { hooks: ["./hooks.json"] };
  }),
  manifestCase("unparseable_mcp", (root) => {
    writeFileSync(join(root, ".mcp.json"), "not json");
    return { mcpServers: "./.mcp.json" };
  }),
  manifestCase("nonparse_failure", (root) => {
    mkdirSync(join(root, "hooks.json"), { recursive: true });
    return { hooks: ["./hooks.json"] };
  }),
  manifestCase("null_manifest", (root) => {
    return { raw: "null" };
  }),
];

// ---- install root (runInstalledRootCheck) ----------------------------------

function payloadAt(name, version, manifest) {
  const root = join(temp, "root-" + name);
  mkdirSync(root, { recursive: true });
  mkdirSync(join(root, ".codex-plugin"), { recursive: true });
  writeFileSync(join(root, ".codex-plugin", "plugin.json"), manifest ?? JSON.stringify({ name: "crw", version }));
  return root;
}
function payloadWithoutManifest(name) {
  const root = join(temp, "root-" + name);
  mkdirSync(root, { recursive: true });
  mkdirSync(join(root, ".codex-plugin"), { recursive: true });
  return root;
}
function homeWith(name, entries) {
  const home = join(temp, "home-" + name);
  mkdirSync(join(home, "plugins", "cache"), { recursive: true });
  for (const [market, plugin, version] of entries) {
    mkdirSync(join(home, "plugins", "cache", market, plugin, version), { recursive: true });
  }
  return home;
}
function installCase(name, run) {
  const check = run();
  return { name, severity: check.severity, evidence: sub(check.evidence), repair: sub(check.repair ?? "") };
}

const installRoot = [
  installCase("matching_root", () => runInstalledRootCheck(payloadAt("matching_root", "0.4.0"), { codexHome: homeWith("matching_root", [["local", "crw", "0.4.0"]]) })),
  installCase("stale_root", () => runInstalledRootCheck(payloadAt("stale_root", "0.4.0"), { codexHome: homeWith("stale_root", [["local", "crw", "0.1.0"]]) })),
  installCase("matching_and_stale", () => runInstalledRootCheck(payloadAt("matching_and_stale", "0.4.0"), { codexHome: homeWith("matching_and_stale", [["local", "crw", "0.1.0"], ["mkt", "crw", "0.4.0"]]) })),
  installCase("two_stale", () => runInstalledRootCheck(payloadAt("two_stale", "0.4.0"), { codexHome: homeWith("two_stale", [["mkt1", "crw", "0.1.0"], ["mkt2", "crw", "0.2.0"]]) })),
  installCase("no_cache", () => runInstalledRootCheck(payloadAt("no_cache", "0.4.0"), { codexHome: join(temp, "empty-home") })),
  installCase("not_installed", () => runInstalledRootCheck(payloadAt("not_installed", "0.4.0"), { codexHome: homeWith("not_installed", [["mkt", "other", "1.0.0"]]) })),
  installCase("no_name_version", () => runInstalledRootCheck(payloadAt("no_name_version", "0.4.0", JSON.stringify({})), { codexHome: homeWith("no_name_version", []) })),
  installCase("manifest_absent", () => runInstalledRootCheck(payloadWithoutManifest("manifest_absent"), { codexHome: homeWith("manifest_absent", []) })),
  installCase("manifest_null", () => runInstalledRootCheck(payloadAt("manifest_null", "0.4.0", "null"), { codexHome: homeWith("manifest_null", []) })),
  installCase("linked_root", () => {
    const home = homeWith("linked_root", [["mkt", "crw", "0.4.0"]]);
    const elsewhere = join(temp, "elsewhere-linked_root");
    mkdirSync(elsewhere, { recursive: true });
    rmSync(join(home, "plugins", "cache", "mkt", "crw", "0.4.0"), { recursive: true });
    symlinkSync(elsewhere, join(home, "plugins", "cache", "mkt", "crw", "0.4.0"));
    return runInstalledRootCheck(payloadAt("linked_root", "0.4.0"), { codexHome: home });
  }),
  installCase("entry_is_file", () => {
    const home = homeWith("entry_is_file", []);
    mkdirSync(join(home, "plugins", "cache", "mkt2"), { recursive: true });
    writeFileSync(join(home, "plugins", "cache", "mkt2", "crw"), "not a dir");
    return runInstalledRootCheck(payloadAt("entry_is_file", "0.4.0"), { codexHome: home });
  }),
  installCase("env_empty", () => {
    const save = process.env.CODEX_HOME;
    process.env.CODEX_HOME = "";
    try { return runInstalledRootCheck(payloadAt("env_empty", "0.4.0"), {}); }
    finally { process.env.CODEX_HOME = save; }
  }),
  installCase("home_empty", () => {
    const savedHome = process.env.HOME;
    const savedCodex = process.env.CODEX_HOME;
    delete process.env.CODEX_HOME;
    process.env.HOME = "";
    try { return runInstalledRootCheck(payloadAt("home_empty", "0.4.0"), {}); }
    finally { process.env.HOME = savedHome; process.env.CODEX_HOME = savedCodex; }
  }),
  installCase("option_wins", () => {
    const savedCodex = process.env.CODEX_HOME;
    process.env.CODEX_HOME = homeWith("option_wins_env", [["mkt", "crw", "0.4.0"]]);
    try { return runInstalledRootCheck(payloadAt("option_wins", "0.4.0"), { codexHome: join(temp, "option-empty-home") }); }
    finally { process.env.CODEX_HOME = savedCodex; }
  }),
  installCase("env_wins_over_home", () => {
    const savedHome = process.env.HOME;
    const savedCodex = process.env.CODEX_HOME;
    process.env.CODEX_HOME = homeWith("env_wins_over_home", [["mkt", "crw", "0.4.0"]]);
    process.env.HOME = join(temp, "home-no-cache");
    try { return runInstalledRootCheck(payloadAt("env_wins_over_home", "0.4.0"), {}); }
    finally { process.env.HOME = savedHome; process.env.CODEX_HOME = savedCodex; }
  }),
	installCase("surrogate_name", () =>
		runInstalledRootCheck(payloadAt("surrogate_name", "0.4.0", JSON.stringify({ name: "x\ud800", version: "0.4.0" })), { codexHome: homeWith("surrogate_name", [["mkt", "x\ufffd", "0.4.0"]]) })),
	installCase("surrogate_missing", () =>
		runInstalledRootCheck(payloadAt("surrogate_missing", "0.4.0", JSON.stringify({ name: "x\ud800", version: "0.4.0" })), { codexHome: homeWith("surrogate_missing", [["mkt", "other", "0.4.0"]]) })),
	installCase("invalid_market_name", () => {
		const home = join(temp, "home-invalid_market_name");
		mkdirSync(join(home, "plugins", "cache"), { recursive: true });
		const market = Buffer.from([0x6d, 0xff]);
		mkdirSync(Buffer.concat([Buffer.from(join(home, "plugins", "cache") + "/"), market, Buffer.from("/crw/0.1.0")]), { recursive: true });
		return runInstalledRootCheck(payloadAt("invalid_market_name", "0.4.0"), { codexHome: home });
	}),
];

// A malformed install manifest: the oracle answers a V8 SyntaxError message the Go port cannot
// reproduce; recorded here so the divergence stays visible in the data.
const malformedManifest = installCase("malformed_manifest", () => runInstalledRootCheck(payloadAt("malformed_manifest", "0.4.0", "not json"), { codexHome: homeWith("malformed_manifest", []) }));

writeFileSync(out, JSON.stringify({
  oracle: "CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)",
  dist: "plugins/codexclaw/components/cxc-ops/dist/doctor.js",
  node: process.version,
  note: "Each group holds the oracle answer for the input the Go test rebuilds, with the recorder's temp root replaced by ${TEMP}. pabcd cases run runDoctor with process.cwd() at the case workspace (checkPabcdHealth is private); an unreadable sessions directory is the oracle's uncaught throw and is recorded as threw. manifestTargets cases run runDoctor over a minimal payload; installRoot cases call runInstalledRootCheck directly. Every evidence string is passed through toWellFormed() at print time by the fixture recorder; these strings are compared in memory, not printed.",
  pabcd,
  manifestTargets,
  installRoot,
  malformedManifest,
}, null, 2) + "\n");
console.log(`recorded ${pabcd.length} pabcd, ${manifestTargets.length} manifest-target, ${installRoot.length} install-root and 1 malformed-manifest case to ${out}`);
