// Node 24 recorder, CXC v0.2.40 (3c1459ac) chat-search.ts:151-268.
// Usage: node record.mjs <read-only-oracle-root> <task-scratch-root> > oracle.json
// Imports pinned sources directly; never changes either oracle tree.
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
const [oracle, scratch] = process.argv.slice(2);
const root = path.join(oracle, 'plugins/codexclaw/components/recall');
const { searchChat } = await import(pathToFileURL(path.join(root, 'src/chat-search.ts')));
const { buildCodexHome } = await import(pathToFileURL(path.join(root, 'test/fixtures.ts')));
const { openIndex } = await import(pathToFileURL(path.join(root, 'src/index-db.ts')));
const now = Date.parse('2026-10-04T12:00:00.000Z');
Date.now = () => now;
fs.mkdirSync(scratch, { recursive: true });
const cases = [];
for (const [name, query, options] of [
  ['cold', 'trigram', {}], ['warm', 'trigram', {}],
  ['read-only', 'trigram', { noRefresh: true }], ['scan', 'trigram', { scan: true }],
  ['empty', '', {}], ['missing', 'trigram', { noRefresh: true }],
  ['invalid-days', '', { days: Infinity }],
  ['query-failure', 'trigram', { noRefresh: true }],
  ['read-only-open', 'trigram', {}],
]) {
  const home = fs.mkdtempSync(path.join(scratch, name + '-'));
  process.env.HOME = home; process.env.CODEX_HOME = home; process.env.CODEXCLAW_HOME = home;
  buildCodexHome(home);
  const indexPath = path.join(home, 'sidecar', 'index.sqlite');
  if (!['cold', 'empty', 'missing', 'invalid-days'].includes(name)) {
    searchChat('trigram', { home, indexPath, nowMs: now });
  }
  if (name === 'query-failure' || name === 'read-only-open') {
    const db = openIndex(indexPath);
    db.exec(name === 'query-failure' ? 'DROP TABLE msgs' : "UPDATE meta SET value='1' WHERE key='schema_version'");
    db.exec('PRAGMA wal_checkpoint(TRUNCATE)'); db.close();
    if (name === 'read-only-open') fs.chmodSync(indexPath, 0o444);
  }
  const opts = { ...options, nowMs: now };
  const record = { name, query, options: opts };
  try {
    const r = searchChat(query, { ...opts, home, indexPath: name === 'missing' ? path.join(home, 'missing.sqlite') : indexPath });
    r.elapsedMs = 0;
    if (r.index?.lastIngestAt) r.index.lastIngestAt = '<INGEST>';
    record.out = JSON.parse(JSON.stringify(r).replaceAll(home, '<HOME>'));
  } catch (e) { record.error = e.message; }
  if (name === 'read-only-open') fs.chmodSync(indexPath, 0o600);
  cases.push(record);
}
console.log(JSON.stringify(cases, null, 2));
