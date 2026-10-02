// Records the CXC v0.2.40 oracle's source identity for the scenario trees of scenarios.json; the Go tests replay
// it from oracle-identities.json (no Node at test time). Each scenario is a shell script run with sh -ec in the
// tree, by this script and by the Go test alike. Recorded with Node v24.20.0 as
//   node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/pabcd-state/dist scenarios.json <short work dir>
// where <oracle> is a read-only tree of CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d).
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join } from "node:path";

const [oracleDist, scenarioFile, workRoot] = process.argv.slice(2);
const { captureSourceIdentity } = await import(oracleDist + "/source-identity.js");
for (const name of Object.keys(process.env)) if (/^GIT_(DIR|WORK_TREE|COMMON_DIR|INDEX_FILE|OBJECT_DIRECTORY|ALTERNATE_OBJECT_DIRECTORIES|CONFIG_COUNT|CONFIG_PARAMETERS)$/.test(name)) delete process.env[name];
Object.assign(process.env, {
  GIT_CEILING_DIRECTORIES: workRoot, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1",
  GIT_AUTHOR_NAME: "fixture", GIT_AUTHOR_EMAIL: "fixture@example.invalid", GIT_AUTHOR_DATE: "2026-01-01T00:00:00Z",
  GIT_COMMITTER_NAME: "fixture", GIT_COMMITTER_EMAIL: "fixture@example.invalid", GIT_COMMITTER_DATE: "2026-01-01T00:00:00Z",
});
const sh = (cwd, script) => execFileSync("sh", ["-ec", script], { cwd, stdio: ["ignore", "pipe", "ignore"] });
const git = (cwd, ...args) => execFileSync("git", args, { cwd, stdio: ["ignore", "pipe", "ignore"] });

const out = {};
for (const sc of JSON.parse(readFileSync(scenarioFile, "utf8"))) {
  const root = mkdtempSync(join(workRoot, "s-"));
  if (sc.base !== "nogit") git(root, "init", "-q", "-b", "main", ".");
  if (!sc.base) {
    mkdirSync(join(root, "sub"));
    writeFileSync(join(root, ".gitignore"), "ignored/\n");
    writeFileSync(join(root, "tracked.ts"), "a\n");
    writeFileSync(join(root, "sub", "x.ts"), "b\n");
    git(root, "add", "-A");
    git(root, "commit", "-qm", "init");
  }
  sh(root, sc.script);
  const dir = join(root, sc.cwd ?? "");
  const id = captureSourceIdentity(dir, sc.options ?? {});
  const rec = { kind: id.kind, commitSha: id.commitSha, dirty: id.dirty };
  if (id.treeHash !== undefined) rec.treeHash = id.treeHash;
  try { rec.statusZ = git(dir, "status", "--porcelain=v1", "-z", "--untracked-files=all").toString("base64"); } catch {}
  out[sc.id] = rec;
  rmSync(root, { recursive: true, force: true });
}
process.stdout.write(JSON.stringify(out, null, 1) + "\n");
