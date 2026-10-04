// CXC v0.2.40 (3c1459ac) helper answers; Node runs only at recording time.
// node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist <scratch>
import { cpSync, mkdirSync, mkdtempSync, writeFileSync, appendFileSync, symlinkSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
const root = mkdtempSync(join(process.argv[3], "memoryrank-"));
const dist = join(root, "dist");
cpSync(fileURLToPath(process.argv[2]), dist, { recursive: true });
writeFileSync(join(dist, "package.json"), '{"type":"module"}\n');
appendFileSync(join(dist, "memory-search.js"), "\nexport { PER_FILE_CAP, RELAXED_PENALTY, listMarkdownFiles, frontmatterThreadId, frontmatterCwd, firstMatchStartLine, groupHit, markGroupPresence, firstPresentMember, excerptAround };\n");
const m = await import(pathToFileURL(join(dist, "memory-search.js")));
const q = await import(pathToFileURL(join(dist, "query-words.js")));
const s = await import(pathToFileURL(join(dist, "synonyms.js")));
const normalize = v => JSON.parse(JSON.stringify(v).split(root).join("$R"));
const cases = [];
const add = (fn, args, call) => {
  const c = { fn, in: normalize(args) };
  try {
    const out = call();
    c.out = normalize(out);
    if (typeof out === "number" && !Number.isFinite(out)) c.number = Number.isNaN(out) ? "nan" : "infinity";
    if (typeof out === "string" && Buffer.from(out, "utf8").toString() !== out) {
      c.classification = "intentionally-changed";
      c.reason = "Go strings cannot hold a lone UTF-16 surrogate; the UTF-8 boundary emits U+FFFD.";
      c.goExpected = Buffer.from(out, "utf8").toString();
    }
  } catch { c.error = "io"; }
  cases.push(c);
};
const kinds = ["summary", "handbook", "skill", "extension", "raw", "rollout", "stage1", "chat", "other", "invalid"];
const now = Date.parse("2026-07-07T00:00:00Z");
add("constants", [], () => [m.DEFAULT_MEMORY_LIMIT, m.PER_FILE_CAP, m.RELAXED_PENALTY, m.CWD_BOOST]);
for (const path of ["memory_summary.md", "MEMORY.md", "raw_memories.md", "skills/a/SKILL.md", "extensions/a.md", "rollout_summaries/a.md", "skills", "memory.md", "other.md", "skills\\a.md", ""]) {
  for (const origin of ["file", "stage1", "chat"]) add("kind", [path, origin], () => m.kindOfRelpath(path, origin));
}
for (const kind of kinds) {
  add("priority", [kind], () => m.KIND_PRIORITY[kind] ?? NaN);
  add("halfLife", [kind], () => m.HALF_LIFE_HOURS[kind] ?? NaN);
  for (const age of [null, -500, 0, 1, 168, 336, 337, 720, 2160, 16800]) {
    const stamp = age === null ? null : now - age * 3_600_000;
    add("recency", [kind, stamp, now], () => m.recencyBoost(kind, stamp, now));
    add("final", [6, kind, stamp, now], () => m.finalScore(6, kind, stamp, now));
  }
}
const words = [["CI"], ["LSP"], ["배포"], ["결정", "세션"], ["2.49.0", "배포"], ["hook.ts", "CI"], ["missing"], []];
const texts = ["", "precision", "ci ci ci_ci CI", "NaiControlsPanel", "the LSP server", "배포 배포 deploy", "# decision session", "결정 세션", "İ CI 😀", "2.49.0 배포", "hook.tsx hook.ts", "deploy ".repeat(20)];
for (const ws of words) {
  const groups = s.expandQueryWords(ws);
  for (const text of texts) {
    const lower = text.toLowerCase();
    add("score", [lower, groups, ws.join(" ").toLowerCase()], () => m.scoreChunk(lower, groups, ws.join(" ").toLowerCase()));
    add("presence", [lower, groups, groups.map((_, i) => i === 0)], () => { const p = groups.map((_, i) => i === 0); m.markGroupPresence(lower, groups, p); return p; });
    add("line", ["intro\r\n\r\n" + text + "\r\n", groups], () => m.firstMatchStartLine("intro\r\n\r\n" + text + "\r\n", groups));
    if (groups.length) {
      add("member", [lower, groups], () => m.firstPresentMember(lower, groups));
      add("group", [lower, groups[0]], () => m.groupHit(lower, groups[0]));
    }
  }
}
for (const doc of ["", "cwd: /a b\n", "thread_id: one\r\ncwd: /a\r\n\r\nbody", "# header\ncwd: /body", "thread_id: one\n\ncwd: /body", "thread_id:\n# next", "Cwd: /bad\ncwd: /later", "bad-key: x\ncwd: /later", "x\rcwd: /a", "x\u2028thread_id: one", "x\u2029thread_id: one", "x\rthread_id: one", "thread_id:\ufeff\u3000one", "thread_id:\u0085x", "😀".repeat(1000) + "\nthread_id: late", "thread_id: " + "x".repeat(1988) + "😀", "cwd: " + "x".repeat(1994) + "😀", "thread_id: " + "x".repeat(1990) + "\ncwd: late"]) {
  add("threadId", [doc], () => m.frontmatterThreadId(doc));
  add("cwd", [doc], () => m.frontmatterCwd(doc));
}
for (const doc of ["", "a\nb\n\nc\n\n\nd\n", "intro\r\n\r\nsecond\r\n\r\nthird\r\n", "\ufeff \n\t\n", "\u0085\n", "a\rb", "\n\n배포\r\n검색\r\n\r\n결정", "x\n"]) add("chunks", [doc], () => m.paragraphChunks(doc));
for (const text of ["precision then CI here", "가나다 CI 라마", "İabcdefgh CI xyz", "ab😀cd", "ΑΣ CI 😀", "no match", "CI", ""]) {
  for (const term of [{ text: "ci", boundary: true }, { text: "ci", boundary: false }, { text: "absent", boundary: false }]) {
    for (const span of [-2, 0, 1, 3, 4, 6, 400]) add("excerpt", [text, term, span], () => m.excerptAround(text, term, span));
  }
}
for (const path of ["mem/MEMORY.md", "mem/nested/a.md", "mem/.hidden.md", "mem/.hidden/a.md", "mem/skip.MD", "mem/other.txt"]) { mkdirSync(join(root, path, ".."), { recursive: true }); writeFileSync(join(root, path), "x"); }
symlinkSync(join(root, "mem/MEMORY.md"), join(root, "mem/link.md"));
symlinkSync(join(root, "mem/nested"), join(root, "mem/link-dir"));
symlinkSync(join(root, "mem"), join(root, "root-link"));
for (const path of ["mem", "missing", "mem/MEMORY.md", "root-link"]) add("list", [join(root, path)], () => m.listMarkdownFiles(join(root, path)));
process.stdout.write(JSON.stringify(cases) + "\n");
