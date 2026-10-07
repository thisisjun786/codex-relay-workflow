/**
 * repo-map-packaging.test.mjs — vendored RepoMapper packaging contract (260706_repo_map).
 *
 * Ported by CRW-939 from CXC v0.2.40 plugins/codexclaw/test/repo-map-packaging.test.mjs with the
 * name substitution of contract/schema/cxc/name-substitution.json: the skill is crw-repo-map and
 * it sits at port/cxc/skills/crw-repo-map, so the paths are the skill's own.
 *
 * The repo-map skill ships a vendored Python script (no TS component, no dist build). This test
 * pins the vendoring contract: required files present, attribution intact, no server file
 * (philosophy no-server rule), load-bearing dependency pins, the tags queries the skill's
 * verification tier names, and the entry point its help text claims.
 *
 * The oracle's four dispatcher cases (cxc map --help exits 0 without python deps; the bootstrap
 * ladder; the win32 venv shape; the win32 py -3 rung) are not ported: they import or spawn
 * bin/codexclaw.mjs, the CXC Node dispatcher this port replaced with the Go crw map command. They
 * are covered by cmd/crw/repomap_test.go (TestRepoMapLadderFromBin for the help bypass, the
 * CRW_PYTHON override, the uv rung, the venv rung and the bare python3 rung with -B everywhere,
 * TestRepoMapBootstrapWithFakesOnly for the bootstrap ladder, TestRepoMapFinalExitAndSpawnErrors
 * and TestRepoMapNoBootstrapUnlessExactOptIn for the rest). The win32 shapes are not ported at
 * all: the port dropped the Windows spawn helpers.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, existsSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const skillDir = resolve(here, "..");
const scriptsDir = join(skillDir, "scripts");

test("vendored python modules exist", () => {
  for (const f of ["repomap.py", "repomap_class.py", "importance.py", "scm.py", "utils.py"]) {
    assert.ok(existsSync(join(scriptsDir, f)), `missing scripts/${f}`);
  }
});

test("MIT license and attribution notice are present", () => {
  const license = readFileSync(join(scriptsDir, "LICENSE"), "utf8");
  assert.match(license, /MIT/);
  const notice = readFileSync(join(scriptsDir, "NOTICE.md"), "utf8");
  assert.match(notice, /RepoMapper/);
  assert.match(notice, /Aider/);
});

test("requirements pin the working parser stack and exclude fastmcp", () => {
  const reqs = readFileSync(join(scriptsDir, "requirements.txt"), "utf8");
  assert.doesNotMatch(reqs, /fastmcp/);
  assert.match(reqs, /tree-sitter-language-pack==0\.9\.0/);
  assert.match(reqs, /tree-sitter==0\.25\.1/);
  assert.match(reqs, /grep-ast==0\.9\.0/);
});

test("no server file is vendored (no-server philosophy)", () => {
  const walk = (dir) => {
    for (const entry of readdirSync(dir)) {
      const p = join(dir, entry);
      if (statSync(p).isDirectory()) walk(p);
      else assert.notEqual(entry, "repomap_server.py", `server file vendored at ${p}`);
    }
  };
  walk(skillDir);
});

test("tags queries cover the fixture-verified languages", () => {
  const queryDirs = [
    join(scriptsDir, "queries", "tree-sitter-language-pack"),
    join(scriptsDir, "queries", "tree-sitter-languages"),
  ].filter(existsSync);
  assert.ok(queryDirs.length > 0, "no queries dirs vendored");
  const all = queryDirs.flatMap((d) => readdirSync(d));
  for (const scm of ["typescript-tags.scm", "python-tags.scm", "rust-tags.scm"]) {
    assert.ok(all.includes(scm), `missing ${scm} in vendored queries`);
  }
});

// The oracle spawned `cxc map --help` and asserted its help text names the real entry point.
// That spawn needs the crw binary, which this job does not build, so the same claim is pinned
// where it lives: argparse's prog, which is what the help text prints.
test("the vendored CLI names its real entry point", () => {
  const script = readFileSync(join(scriptsDir, "repomap.py"), "utf8");
  assert.match(script, /prog="crw map"/, "argparse prog must name the real entry point");
});

test("find_src_files skips compiled-output dirs", () => {
  const script = readFileSync(join(scriptsDir, "repomap.py"), "utf8");
  for (const dir of ["'dist'", "'build'", "'target'", "'out'", "'coverage'"]) {
    assert.ok(script.includes(dir), `skip set must contain ${dir}`);
  }
});

test("skill manifest surface is present and bounded", () => {
  const skillMd = readFileSync(join(skillDir, "SKILL.md"), "utf8");
  assert.ok(skillMd.split("\n").length <= 500, "SKILL.md exceeds 500 lines");
  assert.ok(existsSync(join(skillDir, "agents", "openai.yaml")), "agents/openai.yaml missing");
});

