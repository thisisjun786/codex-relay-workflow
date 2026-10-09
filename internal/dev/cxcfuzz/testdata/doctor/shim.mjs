// The oracle worker for the doctor target (CRW-710). The harness's worker pool starts it as
// "node shim.mjs" and it answers one NDJSON request per line on stdin with one reply per line on
// stdout:
//   {"id":1,"input":<report input>,"root":"<case root>"}  ->  {"id":1,"output":{"text":...,"json":...}}
//   a request it cannot answer                          ->  {"id":1,"error":{"name":...,"message":...}}
//
// The oracle side is CXC v0.2.40 cxc-ops/dist/doctor.js (commit 3c1459ac), imported under
// ORACLE_ROOT the way the record-oracle.mjs recorders import their dist modules:
//   renderDoctor(report)                       the text the command prints
//   JSON.stringify(report, null, 2) + "\n"     the --json document (cli.ts:83-84)
//   buildDeclaredFeaturesCheck(res)            the features check the report appends
//   rollup(checks)                             the report's overall severity
//
// The answer is in the oracle's own names (codexclaw, cxc doctor, cxc enable); the Go side's
// comparison renames it through contract/schema/cxc/name-substitution.json before comparing, so
// this shim must not pre-rename anything.
//
// The input is the same shape the Go side reads: {report: {checks, pluginVersion?, codexVersion?,
// activeSurface?}, run?: {status, stdout, stderr}}. run is the codex features list probe; its
// check is appended to the report's checks exactly as runDoctor appends it (doctor.ts:346).
import { createInterface } from "node:readline";

// The harness always sets ORACLE_ROOT from the target's Oracle.Root (DefaultOracleRoot), so this
// shim carries no host path of its own. It is read per request rather than at load, so a missing
// one answers an error reply instead of killing the worker before the pool can report anything.
const oracleRoot = () => {
  const root = process.env.ORACLE_ROOT;
  if (!root) throw new Error("ORACLE_ROOT is not set: the harness sets it from the target's Oracle.Root");
  return root;
};

// run puts the homes the case declared under its own root, so a shim never reads a real one.
// CODEXCLAW_HOME is the oracle's own state root (CRW_HOME is the port's name for it).
function isolate(root) {
  if (typeof root === "string" && root !== "") {
    process.env.HOME = root + "/home";
    process.env.CODEX_HOME = root + "/codex-home";
    process.env.CRW_HOME = root + "/crw-home";
    process.env.CODEXCLAW_HOME = root + "/codexclaw-home";
    process.env.TMPDIR = root + "/tmp";
  }
}

// answer builds the report the way runDoctor does and renders it both ways. Only the fields the
// input names are set, so an absent optional field stays undefined (the JSON omits it and the
// text drops it) while a present empty string is kept by the JSON.
function answer(doctor, input) {
  const source = input && input.report ? input.report : {};
  const checks = Array.isArray(source.checks) ? source.checks.map(oracleCheck) : [];
  if (input && input.run) {
    checks.push(doctor.buildDeclaredFeaturesCheck({
      status: oracleStatus(input.run.status),
      stdout: input.run.stdout ?? "",
      stderr: input.run.stderr ?? "",
    }));
  }
  const report = {
    schemaVersion: 1,
    overall: doctor.rollup(checks),
    checks,
  };
  for (const key of ["pluginVersion", "codexVersion", "activeSurface"]) {
    if (source[key] !== undefined) report[key] = source[key];
  }
  // The text answer is what the command writes, not the JS string renderDoctor returns: cli.ts
  // does process.stdout.write(renderDoctor(report) + "\\n"), so Node's UTF-8 encoder turns a
  // lone surrogate the report carries into U+FFFD on the way out. Encoding and decoding through
  // a Buffer applies exactly that conversion, so the answer is the printed bytes. The JSON needs
  // no such step: JSON.stringify escapes a lone surrogate as its \\udXXX escape, already ASCII.
  const printed = Buffer.from(doctor.renderDoctor(report), "utf8").toString("utf8");
  return { text: printed, json: JSON.stringify(report, null, 2) + "\n" };
}

// oracleCheck rebuilds one check in the field order the oracle's own code writes it. runDoctor
// builds every check as a literal { name, severity, evidence, repair? } (doctor.ts:25-32 and the
// builders around :263-443), so a check the oracle produces always carries that key order, and
// JSON.stringify preserves it. The input arrives with its keys sorted (the harness canonicalises
// it before sending), so spreading the parsed object would hand JSON.stringify an order the real
// report never has -- and the port's HarnessCheck.MarshalJSON writes the oracle's order, so the
// two documents would differ on key order alone. repair is present only when the input names it,
// which is the oracle's three states (a repair, a present empty repair, no repair).
//
// name, severity and evidence are required and are read as the empty string when the input omits
// them, which is how the port's HarnessCheck reads a missing field (it has no absent state for
// them; only repair is optional). Filling them keeps the two sides reading one malformed check the
// same way. Leaving them undefined would make the oracle print "undefined" and omit the key while
// the port prints an empty string and keeps it, and the shrinker -- which deletes an object's keys
// while it keeps the verdict kind (internal/dev/cxcfuzz/shrink.go reduce) -- could then pin a
// difference it manufactured by deleting a required field instead of the input that really
// differed. No check the oracle's own builders produce can omit these, so filling them cannot hide
// a divergence the oracle can actually reach.
function oracleCheck(c) {
  const out = { name: c.name ?? "", severity: c.severity ?? "", evidence: c.evidence ?? "" };
  if (Object.prototype.hasOwnProperty.call(c, "repair")) out.repair = c.repair;
  return out;
}

// oracleStatus is the probe's exit status as the oracle's runner reports it. The input is written
// in the port's vocabulary, where HarnessRun.Status is -1 (harnessDriftKilled,
// internal/runtime/doctor/harness_drift.go) for a probe that produced no exit code -- killed at
// its timeout, or ended by a signal -- because a real exit code is 0..255. The oracle's spawnSync
// answers null for that same outcome, and undefined for a probe that never started. So -1 maps to
// null: both sides then describe one physical probe result, and the comparison is not measuring
// the two seams' different spellings of it.
function oracleStatus(status) {
  if (status === undefined || status === null || status === -1) return null;
  return status;
}

const lines = createInterface({ input: process.stdin, terminal: false });
lines.on("line", async (line) => {
  const text = line.trim();
  if (text === "") return;
  let request;
  try {
    request = JSON.parse(text);
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: null, error: { name: error.name, message: error.message } }) + "\n");
    return;
  }
  // The worker's start-up handshake, and any input that is not an object, is answered inertly before
  // isolate, the import and answer below: with an empty root the import would read the host's own
  // environment, and a report built from a non-object input is not a case this shim answers
  // (CRW-932). The other shims give their own inert answers for such an input and keep them.
  if (request.input === null || typeof request.input !== "object" || Array.isArray(request.input)) {
    process.stdout.write(JSON.stringify({ id: request.id, output: null }) + "\n");
    return;
  }
  try {
    isolate(request.root);
    const doctor = await import(oracleRoot() + "/cxc-ops/dist/doctor.js");
    process.stdout.write(JSON.stringify({ id: request.id, output: answer(doctor, request.input) }) + "\n");
  } catch (error) {
    process.stdout.write(JSON.stringify({ id: request.id, error: { name: error.name, message: String(error && error.message) } }) + "\n");
  }
});
lines.on("close", () => process.exit(0));
