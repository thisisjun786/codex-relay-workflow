// Records what the CXC v0.2.40 oracle's helper role store (subagent-config/src/store.ts, global scope) does, for the Go tests to replay
// (no Node at test time). The input is oracle-store.json itself: each case's given store and its operations (settings, config, set,
// reset) are read from it and run against a fresh CODEXCLAW_HOME, and the file is rewritten with what the oracle answered: the result
// (JSON.stringify of the settings or config), the error message, and the bytes and modes of the store file after each operation.
// writeConfig is not recorded: it is not ported.
// Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/subagent-config/dist/store.js oracle-store.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, statSync, existsSync, readdirSync, rmSync, chmodSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

const [storeUrl, casesFile] = process.argv.slice(2);
const { readSettings, readConfig, setRole, resetRole } = await import(storeUrl);

const out = [];
for (const c of JSON.parse(readFileSync(casesFile, "utf8"))) {
  const root = mkdtempSync(join(tmpdir(), "role-rec-"));
  const home = join(root, "home"); // CODEXCLAW_HOME
  const cwd = join(root, "ws");
  mkdirSync(cwd, { recursive: true });
  const file = join(home, "subagents.json");
  if (c.given.dirStore) {
    mkdirSync(file, { recursive: true });
    chmodSync(home, 0o755);
  } else if (c.given.store !== undefined && c.given.store !== null) {
    mkdirSync(home, { recursive: true });
    chmodSync(home, 0o755);
    writeFileSync(file, c.given.store);
    chmodSync(file, 0o644);
  }
  const env = { CODEXCLAW_HOME: home };
  const ops = [];
  for (const op of c.ops) {
    const rec = { op: op.op };
    if (op.role !== undefined) rec.role = op.role;
    if (op.patch !== undefined) rec.patch = op.patch;
    try {
      let res;
      if (op.op === "settings") res = readSettings(cwd, "global", env);
      else if (op.op === "config") res = readConfig(cwd, "global", env);
      else if (op.op === "set") res = setRole(cwd, op.role, op.patch, "global", env);
      else if (op.op === "reset") res = resetRole(cwd, op.role, "global", env);
      else throw new Error("unknown op " + op.op);
      rec.result = JSON.stringify(res);
    } catch (err) {
      rec.error = err instanceof Error ? err.message : String(err);
    }
    if (existsSync(file) && statSync(file).isFile()) {
      rec.file = readFileSync(file, "utf8");
      rec.mode = (statSync(file).mode & 0o777).toString(8);
      rec.dirMode = (statSync(home).mode & 0o777).toString(8);
    } else rec.file = null;
    rec.leftovers = existsSync(home) ? readdirSync(home).filter((n) => n.endsWith(".tmp")).length : 0;
    ops.push(rec);
  }
  out.push({ id: c.id, note: c.note, given: c.given, ops });
  rmSync(root, { recursive: true, force: true });
}
writeFileSync(casesFile, JSON.stringify(out, null, 1) + "\n");
