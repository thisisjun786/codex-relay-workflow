// Records the CXC v0.2.40 oracle's answers for the session source binding (session-source.ts,
// session-source-identity.ts, source-gate.ts) over the scenarios of scenarios.json; the Go tests replay them from
// oracle.json (no Node at test time). Each scenario builds the same world (the "world" script, run with sh -ec in a
// fresh case root R) plus its own "setup", then runs its steps in order. Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist scenarios.json <short work dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d). "$R" in a step
// is the case root; a result has R replaced by "$R" so that it does not depend on the directory.
import { mkdtempSync, realpathSync, rmSync, readFileSync, lstatSync, readdirSync, mkdirSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join, dirname } from "node:path";

const [oracleDist, scenarioFile, workDir] = process.argv.slice(2);
const workRoot = realpathSync(workDir);
const { bindSessionSource, resolveSessionSource } = await import(oracleDist + "/session-source.js");
const { captureSessionSourceIdentity } = await import(oracleDist + "/session-source-identity.js");
const { checkBoundSourceIdentity } = await import(oracleDist + "/source-gate.js");
for (const name of Object.keys(process.env)) if (name.startsWith("GIT_")) delete process.env[name];
Object.assign(process.env, {
  GIT_CEILING_DIRECTORIES: workRoot, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1",
  GIT_AUTHOR_NAME: "fixture", GIT_AUTHOR_EMAIL: "fixture@example.invalid", GIT_AUTHOR_DATE: "2026-01-01T00:00:00Z",
  GIT_COMMITTER_NAME: "fixture", GIT_COMMITTER_EMAIL: "fixture@example.invalid", GIT_COMMITTER_DATE: "2026-01-01T00:00:00Z",
});
const sh = (cwd, script) => execFileSync("sh", ["-ec", script], { cwd, stdio: ["ignore", "pipe", "ignore"] });
const { world, scenarios } = JSON.parse(readFileSync(scenarioFile, "utf8"));

const out = {};
for (const sc of scenarios) {
  const R = mkdtempSync(join(workRoot, "s-"));
  const norm = v => JSON.parse(JSON.stringify(v ?? null).split(R).join("$R"));
  const sub = v => JSON.parse(JSON.stringify(v).split("$R").join(R));
  sh(R, world); if (sc.setup) sh(R, sc.setup);
  const results = [];
  for (const raw of sc.steps) {
    const st = sub(raw), cwd = join(R, st.cwd ?? "");
    let res;
    const saved = {};
    for (const [k, v] of Object.entries(st.env ?? {})) { saved[k] = process.env[k]; process.env[k] = v; }
    try {
      switch (st.do) {
        case "bind": res = { ok: bindSessionSource(cwd, st.session, st.rawTarget ?? join(R, st.target)) }; break;
        case "resolve": res = { ok: resolveSessionSource(cwd, st.session) }; break;
        case "capture": {
          const id = captureSessionSourceIdentity(cwd, st.session, st.options ?? {});
          res = { ok: { kind: id.kind, commitSha: id.commitSha, dirty: id.dirty, treeHash: id.treeHash ?? null, sourceRoot: id.sourceRoot ?? null } }; break;
        }
        case "gate": res = { ok: checkBoundSourceIdentity(cwd, st.session) }; break;
        case "state": { const p = join(cwd, ".codexclaw", "sessions", st.session + ".json"); mkdirSync(dirname(p), { recursive: true }); writeFileSync(p, JSON.stringify(st.json)); continue; }
        case "sh": sh(R, st.script); continue;
        case "read": { const p = join(R, st.path); res = { ok: { mode: (lstatSync(p).mode & 0o777).toString(8), content: readFileSync(p, "utf8") } }; break; }
        case "ls": res = { ok: readdirSync(join(R, st.path)).sort() }; break;
      }
    } catch (err) { res = err.code ? { errno: err.code } : { err: err.message }; }
    for (const [k, v] of Object.entries(saved)) { if (v === undefined) delete process.env[k]; else process.env[k] = v; }
    results.push(norm(res));
  }
  out[sc.id] = results;
  rmSync(R, { recursive: true, force: true });
}
process.stdout.write(JSON.stringify(out, null, 1) + "\n");
