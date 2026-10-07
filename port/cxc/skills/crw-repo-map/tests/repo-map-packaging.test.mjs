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

/**
 * The code of each line: a line inside a triple-quoted string is dropped and an unquoted `#` starts
 * a comment, so text that only looks like code -- the vendored script's help epilog holds a bare
 * `Examples:` line -- is never read as a statement. A quote that does not close leaves the rest of
 * the line unread, and the import check then refuses the statement rather than passing it.
 */
function codeLines(source) {
  const out = [];
  let open = null; // the triple-quote delimiter a string is still open with
  for (const raw of source.replace(/\r\n/g, "\n").split("\n")) {
    let line = raw;
    if (open !== null) {
      const end = line.indexOf(open);
      if (end < 0) {
        out.push("");
        continue;
      }
      line = line.slice(end + open.length);
      open = null;
    }
    let code = "";
    let at = 0;
    while (at < line.length) {
      const ch = line[at];
      if (ch === "#") break;
      if (ch === '"' || ch === "'") {
        const triple = ch.repeat(3);
        if (line.slice(at, at + 3) === triple) {
          const end = line.indexOf(triple, at + 3);
          if (end < 0) {
            open = triple;
            break;
          }
          at = end + 3;
          continue;
        }
        const end = line.indexOf(ch, at + 1);
        if (end < 0) break; // a quote that does not close: the rest of the line is unread
        at = end + 1;
        continue;
      }
      code += ch;
      at += 1;
    }
    out.push(code);
  }
  return out;
}

/**
 * True when the import at `code[i]` sits inside a `def` body, which is what defers it past the
 * parse. The enclosing blocks are walked by indentation: a `def` header defers the import, another
 * header (`class`, `if`, `try`, `with`, `for`, ...) is stepped over and the walk continues, and a
 * plain statement at a smaller indent -- or the top of the file -- means module level, which runs
 * while the module loads (CRW-939, the generation-2 evaluations). A `class` body is stepped over
 * rather than treated as deferring: a module-level class body runs when the class statement runs,
 * which is while the module loads, and only a `class` nested in a `def` is deferred with it.
 */
function deferredImport(code, i) {
  let indent = code[i].match(/^[ \t]*/)[0].length;
  for (let j = i - 1; j >= 0; j--) {
    if (code[j].trim() === "") continue;
    const narrower = code[j].match(/^[ \t]*/)[0].length;
    if (narrower >= indent) continue;
    const text = code[j].trim();
    if (/^(?:async[ \t]+def|def)[ \t]/.test(text)) return true;
    if (/^(?:class|if|elif|else|try|except|finally|with|for|while|match|case)\b/.test(text) && /:[ \t]*$/.test(text)) {
      indent = narrower;
      continue;
    }
    return false;
  }
  return false;
}

/** Imports of the optional parser stack that run before the `parse_args()` call. */
/**
 * The import statements of one line of code, together with the compound header a statement shares
 * its line with. Python allows several statements on one line: a `;` separates them, and a
 * `if`/`def`/... header may be followed by its body on the same line. A line that merely begins
 * with something else can therefore still carry an import this check has to read (CRW-939, the
 * third generation-2 evaluation's d2). Strings and comments are already gone when this runs.
 */
function importStatements(line) {
  const out = [];
  for (const part of line.split(";")) {
    const text = part.trim();
    if (/^(?:from|import)[ \t]/.test(text)) {
      out.push({ text, header: null });
      continue;
    }
    const inline = /^(.*?):[ \t]*((?:from|import)[ \t].*)$/.exec(text);
    if (inline) out.push({ text: inline[2].trim(), header: inline[1].trim() });
  }
  return out;
}

/** True when a compound header defers the statement it shares its line with past the parse. */
function inlineHeaderDefers(header) {
  return header !== null && /^(?:async[ \t]+def|def)[ \t]/.test(header);
}

function parserImportsBeforeParsing(source) {
  const code = codeLines(source);
  const parseAt = code.findIndex((line) => /^\s*args\s*=\s*parser\.parse_args\(\)\s*$/.test(line));
  if (parseAt < 0) return { parseAt, offenders: ["(no `args = parser.parse_args()` line found)"] };
  const offenders = [];
  for (const [i, line] of code.entries()) {
    for (const statement of importStatements(line)) {
      const text = statement.text;
      // `parse_args()` lives inside main(), so an import deferred inside a def body placed after
      // the parse runs only once that body is reached; every other import runs while the module
      // loads or before the parse in main(), so it precedes --help. An import under a module-level
      // compound header (`if True:` / `try:` / `with`) runs while the module loads and is not
      // deferred, and only a `def` header defers a statement it shares its line with.
      if (i >= parseAt && (deferredImport(code, i) || inlineHeaderDefers(statement.header))) continue;
      const modules = importedModules(text);
      if (modules === null) {
        offenders.push(`line ${i + 1}: ${text} (not a plain import this check can read)`);
        continue;
      }
      // Every module the statement binds, not only the first: `import os, networkx` names both.
      if (modules.some((mod) => PARSER_IMPORTS.includes(mod))) offenders.push(`line ${i + 1}: ${text}`);
    }
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
  // The control: the same import inside the function body after the parse is deferred past it and
  // stays clean.
  const inFunction = "def main():\n    args = parser.parse_args()\n    from repomap_class import RepoMap\n";
  assert.deepEqual(parserImportsBeforeParsing(inFunction).offenders, [], "a deferred import must stay clean");
  // A module-level `try`/`except` around the parse is the real file's shape: the import below the
  // parse inside that try is deferred, and the check must not report it.
  const tryShape = "def main():\n    args = parser.parse_args()\n    try:\n        from repomap_class import RepoMap\n    except ImportError:\n        pass\n";
  assert.deepEqual(parserImportsBeforeParsing(tryShape).offenders, [], "a deferred try import must stay clean");
  // Indentation alone does not make an import deferred: a module-level compound statement
  // (`if True:` / `try:` / a `with`) runs while the module loads, so an import indented under one
  // still executes before main() (CRW-939, the generation-2 evaluation's d3).
  for (const shape of ["if True:\n    import networkx\n", "try:\n    import networkx\nexcept ImportError:\n    pass\n", "with open('x'):\n    import networkx\n"]) {
    const { offenders: got } = parserImportsBeforeParsing(parse + shape);
    assert.ok(got.length > 0, `${JSON.stringify(shape)}: an import under a module-level block runs before the parse`);
  }
  // A `class` body is not deferred either: a module-level class statement runs while the module
  // loads, so an optional import inside one placed after the parse still runs before main(). Only a
  // `class` nested inside a `def` is deferred with it (CRW-939, the second generation-2 evaluation).
  const classShape = "class Probe:\n    import networkx\n";
  assert.ok(parserImportsBeforeParsing(parse + classShape).offenders.length > 0,
    "an import in a module-level class body runs before the parse");
  const classInFunction = "def main():\n    args = parser.parse_args()\n    class Probe:\n        import networkx\n";
  assert.deepEqual(parserImportsBeforeParsing(classInFunction).offenders, [],
    "a class body inside the function after the parse stays clean");
  // A line can carry more than one statement: a `;` separates two, and a compound header may be
  // followed by its body on the same line. An import in either position runs at the same time as
  // the statement it shares the line with, so the check reads it (CRW-939, the third
  // generation-2 evaluation's d2).
  for (const shape of [
    "os = 1; import networkx\n",
    "if True: import networkx\n",
    "try: import networkx\nexcept ImportError:\n    pass\n",
    "with open('x'): import networkx\n",
  ]) {
    const { offenders: got } = parserImportsBeforeParsing(parse + shape);
    assert.ok(got.length > 0, `${JSON.stringify(shape)}: an import on a shared line runs before the parse`);
  }
  // The control: the same shapes inside the function after the parse are deferred and stay clean.
  for (const shape of [
    "def main():\n    args = parser.parse_args()\n    if True: import networkx\n",
    "def main():\n    args = parser.parse_args()\n    os = 1; import networkx\n",
  ]) {
    assert.deepEqual(parserImportsBeforeParsing(shape).offenders, [],
      `${JSON.stringify(shape)}: a statement inside the function after the parse stays clean`);
  }
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
