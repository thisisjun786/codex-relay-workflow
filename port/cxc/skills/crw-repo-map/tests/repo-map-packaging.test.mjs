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

// The oracle spawned `cxc map --help` and asserted exit 0 with no python deps. That spawn needs the
// crw binary and an interpreter, and this repository's CI runs no skill script, so the same claim
// is checked from the source structure instead: the modules that pull the optional parser
// dependencies in are imported only after `parse_args()` has answered `--help`.
//
// `parse_args()` is what prints help and exits; an import above it runs first, so a parser import
// moved ahead of the parse makes `--help` fail without the deps and this test is what sees it. The
// list is every optional dependency of the vendored stack plus the two local modules that import
// them at their own top level (`utils`, `repomap_class`).
const PARSER_IMPORTS = ["diskcache", "networkx", "grep_ast", "tree_sitter", "tree_sitter_language_pack", "tiktoken", "pygments", "utils", "repomap_class"];

/**
 * The modules one import statement binds, without following anything: a plain `import a, b.c`
 * statement and a `from a import b, c` statement both name every module they touch. Anything the
 * reader cannot fully parse (a parenthesised or continued statement, an `importlib` call, a
 * relative import) is reported as an offender, so the check fails closed rather than passing on a
 * line it did not read.
 */
function importedModules(statement) {
  const text = statement.trim();
  const from = /^from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+import\s+(.+)$/.exec(text);
  if (from) return [from[1].split(".")[0]];
  const plain = /^import\s+(.+)$/.exec(text);
  if (!plain) return null;
  const names = plain[1].split(",").map((part) => part.trim().split(/\s+as\s+/)[0].trim());
  if (names.some((name) => !/^[A-Za-z_][A-Za-z0-9_.]*$/.test(name))) return null;
  return names.map((name) => name.split(".")[0]);
}

/** Imports of the optional parser stack that appear before the `parse_args()` call. */
function parserImportsBeforeParsing(source) {
  const lines = source.replace(/\r\n/g, "\n").split("\n");
  const parseAt = lines.findIndex((line) => /^\s*args\s*=\s*parser\.parse_args\(\)\s*$/.test(line));
  if (parseAt < 0) return { parseAt, offenders: ["(no `args = parser.parse_args()` line found)"] };
  const offenders = [];
  for (const [i, line] of lines.entries()) {
    const code = line.trim();
    if (!/^(?:from|import)\s/.test(code)) continue;
    // `parse_args()` lives inside main(), so it runs only after the whole module body has
    // executed. A module-level import (no indentation) therefore precedes the parse however late
    // it sits in the file, while an import indented inside a function can be deferred past it.
    // Reading only the lines before the textual parse missed the module-level shape: an import
    // moved just above the `if __name__ == "__main__":` guard still sat after the parse in the
    // text, but a module-level import runs before main() and makes `--help` fail without the
    // deps (CRW-939, the generation-2 evaluation's d2).
    const moduleLevel = !/^[ \t]/.test(line);
    if (!moduleLevel && i >= parseAt) continue;
    const modules = importedModules(code);
    if (modules === null) {
      offenders.push(`line ${i + 1}: ${code} (not a plain import this check can read)`);
      continue;
    }
    // Every module the statement binds, not only the first: `import os, networkx` names both.
    if (modules.some((mod) => PARSER_IMPORTS.includes(mod))) offenders.push(`line ${i + 1}: ${code}`);
  }
  return { parseAt, offenders };
}

test("the vendored CLI names its real entry point", () => {
  const script = readFileSync(join(scriptsDir, "repomap.py"), "utf8");
  assert.match(script, /prog="crw map"/, "argparse prog must name the real entry point");
});

test("repomap.py parses its arguments before it imports the optional parser stack", () => {
  const source = readFileSync(join(scriptsDir, "repomap.py"), "utf8");
  const { parseAt, offenders } = parserImportsBeforeParsing(source);
  assert.ok(parseAt >= 0, "repomap.py must call parser.parse_args()");
  assert.deepEqual(offenders, [], `an optional parser import runs before --help can answer:\n${offenders.join("\n")}`);
  // The deferred block really is there: the same modules are imported after the parse.
  const after = source.replace(/\r\n/g, "\n").split("\n").slice(parseAt).join("\n");
  for (const mod of ["utils", "repomap_class"]) {
    assert.match(after, new RegExp(`from ${mod} import`), `${mod} must still be imported after the parse`);
  }
});

// Red-first control: moving one deferred import above `parse_args()` is exactly the regression the
// oracle's spawn would have caught, and the structural check has to catch it without a Python run.
test("the parser-import check sees an import moved above the argument parsing", () => {
  const source = readFileSync(join(scriptsDir, "repomap.py"), "utf8");
  const lines = source.replace(/\r\n/g, "\n").split("\n");
  const deferredAt = lines.findIndex((line) => line.trim() === "from repomap_class import RepoMap");
  assert.ok(deferredAt > 0, "repomap.py must defer `from repomap_class import RepoMap`");
  const parseAt = lines.findIndex((line) => /^\s*args\s*=\s*parser\.parse_args\(\)\s*$/.test(line));
  assert.ok(parseAt > 0 && deferredAt > parseAt, "the import must start out after the parse");
  const moved = [...lines.slice(0, parseAt), lines[deferredAt], ...lines.slice(parseAt, deferredAt), ...lines.slice(deferredAt + 1)];
  const { offenders } = parserImportsBeforeParsing(moved.join("\n"));
  assert.equal(offenders.length, 1, `the moved import must be reported once, got ${JSON.stringify(offenders)}`);
  assert.match(offenders[0], /from repomap_class import RepoMap$/, "the moved import must be named");
  // The unmodified source is the control: the same reader reports nothing for it.
  assert.deepEqual(parserImportsBeforeParsing(source).offenders, [], "the unmodified source must be clean");
});

// The check reads every module an import statement names, and it fails closed on a statement it
// cannot read, so a dependency cannot slip past inside a multi-module or unparseable import.
test("the parser-import check reads every module and refuses what it cannot read", () => {
  const parse = "args = parser.parse_args()\n";
  for (const line of [
    "import os, networkx\n",                     // the second module is the parser dependency
    "import os, networkx as nx\n",
    "import diskcache, os\n",
    "from grep_ast import TreeContext\n",
    "from os import path\n",                      // the module itself is not a dependency
    "import os\n",
  ]) {
    const { offenders } = parserImportsBeforeParsing(line + parse);
    const wantsFinding = /networkx|diskcache|grep_ast/.test(line);
    assert.equal(offenders.length > 0, wantsFinding, `${JSON.stringify(line)}: got ${JSON.stringify(offenders)}`);
  }
  for (const line of [
    "from repomap_class import (RepoMap,\n", // a continued statement this reader does not parse
    "from . import utils\n",                 // a relative import it cannot resolve
  ]) {
    const { offenders } = parserImportsBeforeParsing(line + parse);
    assert.ok(offenders.length > 0, `${JSON.stringify(line)}: an unreadable import must be refused, not passed`);
  }
});

// `parse_args()` runs inside main(), so a module-level import placed after the parse in the text
// still executes before the parse and breaks `--help` without the parser deps. The check has to
// read module-level imports wherever they sit, not only above the parse line (CRW-939, the
// generation-2 evaluation's d2).
test("the parser-import check reads module-level imports placed after the parse", () => {
  const parse = "args = parser.parse_args()\n";
  const moduleLevel = "from repomap_class import RepoMap\n"; // column 0: runs before main()
  const { offenders } = parserImportsBeforeParsing(parse + moduleLevel);
  assert.ok(offenders.length > 0, "a module-level parser import after the parse must still be reported");
  // The control: the same import indented inside the function is deferred past the parse and clean.
  const deferred = "    from repomap_class import RepoMap\n";
  assert.deepEqual(parserImportsBeforeParsing(parse + deferred).offenders, [], "a deferred import must stay clean");
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
