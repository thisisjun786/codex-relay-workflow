/**
 * repo-map-smoke.test.mjs — fixture smoke run for the vendored RepoMapper (260706_repo_map).
 *
 * Runs the vendored RepoMapper against tiny TS/Python/Rust fixtures and asserts the
 * known symbols surface in the ranked map. Skips cleanly when the optional Python
 * deps are not installed (the tool itself degrades to an install hint).
 *
 * Ported by CRW-939 from CXC v0.2.40 plugins/codexclaw/test/repo-map-smoke.test.mjs, with the
 * name substitution of contract/schema/cxc/name-substitution.json. Three adaptations:
 *   - the fixtures are written into a temporary directory at run time instead of being committed,
 *     because a .py file may sit only in a skill's scripts/ or examples/ (crw-dev ci validate).
 *     They are the same three files the oracle kept under plugins/codexclaw/test/fixtures/repo-map/.
 *   - the interpreter is the one the deps are already installed in (CRW_PYTHON, else python3), not
 *     a `uv run --with-requirements` resolve. The oracle's check also accepted a bare `uv` on PATH
 *     because it drove `cxc map`, whose ladder would resolve the deps itself; this test runs the
 *     vendored script directly, so an interpreter that cannot import them is the only runnable case.
 *     The skill-scripts-node job installs neither, so it reports the test skipped, exactly as the
 *     oracle's own CI did with its CODEXCLAW_SKIP_REPOMAP_SMOKE escape.
 *   - CRW_SKIP_REPOMAP_SMOKE=1 skips it, the renamed oracle escape.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const scriptsDir = resolve(here, "..", "scripts");
const python = process.env.CRW_PYTHON || "python3";

/** The oracle's fixtures/repo-map/{sample.ts,sample.py,sample.rs}, verbatim. */
const FIXTURES = {
  "sample.ts": [
    'import { gammaHelper } from "./sample_helpers";',
    "",
    "export interface BetaShape {",
    "  id: string;",
    "  weight: number;",
    "}",
    "",
    "export function alphaFn(shape: BetaShape): string {",
    "  return gammaHelper(shape.id);",
    "}",
    "",
  ].join("\n"),
  "sample.py": [
    "class DeltaStore:",
    "    def __init__(self):",
    "        self.items = {}",
    "",
    "    def put(self, key, value):",
    "        self.items[key] = value",
    "",
    "",
    "def gamma_helper(key):",
    "    store = DeltaStore()",
    "    store.put(key, True)",
    "    return key",
    "",
  ].join("\n"),
  "sample.rs": [
    "pub struct ZetaConfig {",
    "    pub retries: u32,",
    "}",
    "",
    "pub fn epsilon_run(config: &ZetaConfig) -> u32 {",
    "    config.retries + 1",
    "}",
    "",
    "pub fn epsilon_main() -> u32 {",
    "    let config = ZetaConfig { retries: 2 };",
    "    epsilon_run(&config)",
    "}",
    "",
  ].join("\n"),
};

function depsAvailable() {
  // The ladder's last rung: the interpreter must already have the imports. Nothing here installs them.
  return spawnSync(python, ["-c", "import grep_ast, networkx, diskcache"], { encoding: "utf8" }).status === 0;
}

test("the vendored map surfaces fixture symbols across TS/Python/Rust", (t) => {
  if (process.env.CRW_SKIP_REPOMAP_SMOKE === "1") {
    t.skip("CRW_SKIP_REPOMAP_SMOKE=1 (avoid a live dependency resolve)");
    return;
  }
  if (!depsAvailable()) {
    t.skip(`python deps not installed for ${python}; the vendored map degrades to an install hint`);
    return;
  }
  const dir = mkdtempSync(join(tmpdir(), "crw-repomap-smoke-"));
  try {
    const fixturesDir = join(dir, "fixtures");
    const cwd = join(dir, "cwd");
    mkdirSync(fixturesDir, { recursive: true });
    mkdirSync(cwd, { recursive: true });
    for (const [name, text] of Object.entries(FIXTURES)) writeFileSync(join(fixturesDir, name), text);
    const cacheDir = join(dir, "cache");
    const res = spawnSync(
      python,
      ["-B", join(scriptsDir, "repomap.py"), fixturesDir, "--budget", "1024"],
      { encoding: "utf8", cwd, env: { ...process.env, CRW_REPOMAP_CACHE: cacheDir } },
    );
    assert.equal(res.status, 0, `stderr: ${res.stderr}`);
    assert.match(res.stdout, /alphaFn/);
    assert.match(res.stdout, /gamma_helper/);
    assert.match(res.stdout, /epsilon_run/);
    // The upstream RepoMapper left a .repomap.tags.cache directory in the working directory; the
    // vendored copy must keep its derived cache where CRW_REPOMAP_CACHE says instead.
    const strays = readdirSync(cwd).filter((f) => f.startsWith(".repomap.tags.cache"));
    assert.deepEqual(strays, [], "stray upstream cache dir created in cwd");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
