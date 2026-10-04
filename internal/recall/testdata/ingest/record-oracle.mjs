// CXC v0.2.40, commit 3c1459ac. Node 24 records; Go tests replay without Node.
// node record-oracle.mjs <oracle-recall-dist-url> <owned-scratch-root>
import { mkdirSync, writeFileSync, readFileSync, unlinkSync, utimesSync } from "node:fs";
import { join } from "node:path";
const { ingest, measureIndexFreshness } = await import(process.argv[2] + "/ingest.js");
const { openIndex } = await import(process.argv[2] + "/index-db.js");
const timestamp = "2026-01-01T00:00:00Z", epoch = new Date(timestamp);
const line = payload => JSON.stringify({ type: "response_item", timestamp, payload }) + "\n";
const message = text => line({ type: "message", role: "user", content: [{ type: "input_text", text }] });
const meta = JSON.stringify({ type: "session_meta", payload: { id: "thread", cwd: "/project", git: { repository_url: "https://example.test/team/repo.git" } } }) + "\n";
const path = "sessions/2026/01/01/main.jsonl";
const cases = [
  { name: "lifecycle", files: { [path]: meta + message("original 한글") }, actions: [
    { kind: "ingest" }, { kind: "ingest" },
    { kind: "append", path, data: message("appended quokka") + message("partial 한글").slice(0, -2) },
    { kind: "ingest" }, { kind: "append", path, data: message("partial 한글").slice(-2) },
    { kind: "ingest" }, { kind: "write", path, data: meta + message("new") },
    { kind: "ingest" }, { kind: "remove", path }, { kind: "ingest" },
  ] },
  { name: "growth-rewrite-kept", files: { [path]: message("original") }, actions: [
    { kind: "ingest" }, { kind: "write", path, data: message("rewritten prefix that is longer") + message("later message") }, { kind: "ingest" },
  ] },
  { name: "same-fingerprint-rewrite-kept", files: { [path]: message("first") }, actions: [
    { kind: "ingest" }, { kind: "write", path, data: message("other"), sameMtime: true }, { kind: "ingest" },
    { kind: "write", path, data: message("other") }, { kind: "ingest" },
  ] },
  { name: "utf16-cap", files: { [path]: meta + line({ type: "function_call_output", output: "a".repeat(8191) + "😀tail" }) }, actions: [
    { kind: "ingest" },
  ] },
  { name: "no-complete-line", files: { [path]: meta.trimEnd() }, actions: [
    { kind: "ingest" }, { kind: "append", path, data: "\n" + message("complete") }, { kind: "ingest" },
  ] },
];
for (const [i, c] of cases.entries()) {
  const home = join(process.argv[3], String(i), "codex");
  const write = (p, data, ms = epoch.getTime()) => {
    const target = join(home, p);
    mkdirSync(join(target, ".."), { recursive: true });
    writeFileSync(target, data);
    utimesSync(target, new Date(ms), new Date(ms));
  };
  for (const [p, data] of Object.entries(c.files)) write(p, data);
  const db = openIndex(join(process.argv[3], String(i), "index.sqlite"));
  c.expected = [];
  let mtime = epoch.getTime();
  for (const a of c.actions) {
    if (a.kind === "write" || a.kind === "append") {
      if (!a.sameMtime) mtime += 2000;
      write(a.path, a.kind === "append" ? readFileSync(join(home, a.path), "utf8") + a.data : a.data, mtime);
    } else if (a.kind === "remove") unlinkSync(join(home, a.path));
    else {
      const result = ingest(home, db, 0);
      result.elapsedMs = 0;
      const normal = rows => rows.map(r => ({ ...r, path: r.path.slice(home.length + 1) }));
      c.expected.push({ result, freshness: measureIndexFreshness(home, db, 0),
        files: normal(db.prepare("SELECT path,mtime_ms,size,thread_id,cwd,source,date,bytes_ingested,last_ord,repo_key FROM files ORDER BY path").all()),
        msgs: normal(db.prepare("SELECT path,ord,ts,role,match_field,synthetic,text FROM msgs ORDER BY path,ord").all()) });
    }
  }
  db.close();
}
process.stdout.write(JSON.stringify(cases, null, 2) + "\n");
