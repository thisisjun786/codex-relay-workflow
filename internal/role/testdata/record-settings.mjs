// Records what the CXC v0.2.40 oracle's settings API and spawn resolution (subagent-config/src/settings-api.ts and store.ts
// resolveSpawnConfig, global scope) do, for the Go tests to replay (no Node at test time). The input is oracle-settings.json itself: each
// case's given store and operations (get, update, get_response, update_response, resolve) are run against a fresh CODEXCLAW_HOME, and
// the file is rewritten with what the oracle answered: the result (JSON.stringify of what the call returned), the error message, and the
// bytes of the store after each operation. An update body or a get scope is JSON text, parsed by JSON.parse like a request body.
// Recorded with Node v24.20.0 as
//   node record-settings.mjs file://<oracle>/plugins/codexclaw/components/subagent-config/dist file://... oracle-settings.json
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d) that holds the compiled dist/.
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

const [distUrl, casesFile] = process.argv.slice(2);
const store = await import(distUrl + "/store.js");
const api = await import(distUrl + "/settings-api.js");

const out = [];
for (const c of JSON.parse(readFileSync(casesFile, "utf8"))) {
  const root = mkdtempSync(join(tmpdir(), "role-settings-rec-"));
  const home = join(root, "home"); // CODEXCLAW_HOME
  const cwd = join(root, "ws");
  mkdirSync(cwd, { recursive: true });
  const file = join(home, "subagents.json");
  if (c.given.store !== null && c.given.store !== undefined) {
    mkdirSync(home, { recursive: true });
    writeFileSync(file, c.given.store);
  }
  process.env.CODEXCLAW_HOME = home; // updateSettings and getSettings read the process environment
  const ops = [];
  for (const op of c.ops) {
    const rec = { ...op };
    const get = () => api.getSettings(cwd, op.scope === undefined ? undefined : JSON.parse(op.scope));
    const update = () => api.updateSettings(cwd, JSON.parse(op.body));
    try {
      if (op.op === "get") rec.result = JSON.stringify(get());
      else if (op.op === "update") rec.result = JSON.stringify(update());
      else if (op.op === "get_response") rec.result = JSON.stringify(api.settingsResponse(get));
      else if (op.op === "update_response") rec.result = JSON.stringify(api.settingsResponse(update));
      else if (op.op === "resolve") rec.result = JSON.stringify(store.resolveSpawnConfig(cwd, op.role, { CODEXCLAW_HOME: home }));
      else throw new Error("unknown op " + op.op);
    } catch (err) {
      rec.error = err instanceof Error ? err.message : String(err);
    }
    rec.file = existsSync(file) ? readFileSync(file, "utf8") : null;
    if (existsSync(join(cwd, ".codexclaw"))) throw new Error("case " + c.id + " wrote a project file: the body must name scope global");
    ops.push(rec);
  }
  out.push({ id: c.id, note: c.note, given: c.given, ops });
  rmSync(root, { recursive: true, force: true });
}
writeFileSync(casesFile, JSON.stringify(out, null, 1) + "\n");
