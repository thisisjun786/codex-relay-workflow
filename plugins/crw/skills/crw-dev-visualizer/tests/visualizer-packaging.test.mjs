// visualizer-packaging.test.mjs — issues #182 and #183.
//
// dev-visualizer shipped four kinds of reference that a standalone consumer cannot
// resolve: sibling links into ../dev/, a prose dependency on the same file that a
// Markdown link checker would miss, and repository-only devlog pointers. The skill
// looked fine in this repo and broke the moment it was copied on its own.
//
// The skill's one sibling dependency is the reader-document contract of crw-dev
// (references/reader-documents.md), which is kept in one place. These tests operate on an
// ISOLATED COPY of the skill directory plus that one file, because that is the only way to
// observe the defect: inside the repo, every one of those paths resolves.
import { test } from "node:test";
import assert from "node:assert/strict";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const SKILL = join(here, "..");
const CANONICAL = join(here, "..", "..", "crw-dev", "references", "reader-documents.md");
// The only file outside the skill root that its links may reach, relative to the skills directory.
const SIBLING = join("crw-dev", "references", "reader-documents.md");

function markdownFiles(root) {
  const out = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) walk(full);
      else if (entry.endsWith(".md")) out.push(full);
    }
  };
  walk(root);
  return out;
}

/** Local markdown link targets, with anchors and external schemes dropped. */
function localLinks(text) {
  const out = [];
  for (const m of text.matchAll(/\]\(([^)\s]+)\)/g)) {
    const raw = m[1];
    if (/^[a-z]+:/i.test(raw) || raw.startsWith("#")) continue;
    out.push(raw.split("#")[0]);
  }
  return out.filter(Boolean);
}

function isolatedCopy() {
  const dir = mkdtempSync(join(tmpdir(), "crw-viz-standalone-"));
  const dest = join(dir, "skills", "crw-dev-visualizer");
  cpSync(SKILL, dest, { recursive: true });
  mkdirSync(dirname(join(dir, "skills", SIBLING)), { recursive: true });
  cpSync(CANONICAL, join(dir, "skills", SIBLING));
  return { dir, dest };
}

test("#183: every local markdown link resolves inside an isolated single-skill copy", () => {
  const { dir, dest } = isolatedCopy();
  try {
    const broken = [];
    for (const file of markdownFiles(dest)) {
      for (const link of localLinks(readFileSync(file, "utf8"))) {
        const target = resolve(dirname(file), link);
        if (!existsSync(target)) broken.push(relative(dest, file) + " -> " + link);
      }
    }
    assert.deepEqual(broken, [], "links that do not resolve standalone:\n" + broken.join("\n"));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("#183: no required reference escapes the skill root but the reader-document contract", () => {
  const { dir, dest } = isolatedCopy();
  try {
    const skills = dirname(dest);
    const escapes = [];
    for (const file of markdownFiles(dest)) {
      for (const link of localLinks(readFileSync(file, "utf8"))) {
        const target = resolve(dirname(file), link);
        const rel = relative(dest, target);
        if ((rel.startsWith("..") || rel.startsWith(".." + sep)) && relative(skills, target) !== SIBLING) {
          escapes.push(relative(dest, file) + " -> " + link);
        }
      }
    }
    assert.deepEqual(escapes, [], "references outside the skill root:\n" + escapes.join("\n"));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("#182: no shipped file points at a private devlog path", () => {
  const { dir, dest } = isolatedCopy();
  try {
    const hits = [];
    const walk = (d) => {
      for (const entry of readdirSync(d)) {
        const full = join(d, entry);
        if (statSync(full).isDirectory()) walk(full);
        else if (readFileSync(full, "utf8").includes("devlog" + "/_plan")) hits.push(relative(dest, full));
      }
    };
    walk(dest);
    assert.deepEqual(hits, [], "shipped files citing an unshipped ledger:\n" + hits.join("\n"));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("#183: the reader contract is the one copy of crw-dev, linked and not restated", () => {
  assert.equal(existsSync(join(SKILL, "references", "reader-documents.md")), false, "the skill carries its own copy of the reader contract");
  const canonical = readFileSync(CANONICAL, "utf8");
  const rules = [...canonical.matchAll(/^##\s+(READER-DOC-\d+)/gm)].map((m) => m[1]);
  assert.equal(rules.length, 5, "the reader contract names READER-DOC-01..05");
  assert.match(readFileSync(join(SKILL, "SKILL.md"), "utf8"), /\]\(\.\.\/crw-dev\/references\/reader-documents\.md\)/, "SKILL.md links the one reader contract");
  // Attribution as owner is the reason the other skills point here instead of restating it.
  assert.match(canonical, /Canonical owner/, "the reader contract must name itself the canonical owner");
});

test("#182: the provenance summaries carry their dates and stay scoped", () => {
  const text = readFileSync(join(SKILL, "references", "print-provenance.md"), "utf8");
  for (const anchor of ["{#chromium-probe}", "{#cjk-typography}", "{#report-writing}"]) {
    assert.ok(text.includes(anchor), "missing anchor " + anchor);
  }
  assert.match(text, /2026-09-09/, "a measurement without its date is not provenance");
  assert.match(text, /not .*guarantee|Scope:/i, "a dated observation must not read as an ongoing guarantee");
});
