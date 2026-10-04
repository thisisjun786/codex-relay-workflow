// Node 24 records CXC v0.2.40 (3c1459ac) searchMemory; Go replays without Node.
// node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist <scratch>
// Oracle trees are read-only. Every input is synthetic and rebuilt by the Go test.
import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
const { searchMemory } = await import(process.argv[2] + '/memory-search.js');
const root = fs.mkdtempSync(path.join(process.argv[3], 'memorysearch-'));
const now = Date.parse('2026-09-10T00:00:00Z'), day = 86400000;
const file = (path, content, mtime = now) => ({ path, content, mtime });
const cases = [];
const add = (name, files, query, options = {}, extra = {}) => cases.push({ name, files, query, options: { nowMs: now, ...options }, ...extra });
const basic = [
  file('MEMORY.md', '# Task Group: search infrastructure\n\n## Task 1: ship the trigram sidecar index, success\n\nkeywords: trigram, sidecar, 한글 검색\n'),
  file('windows-notes.md', '# CRLF notes\r\n\r\nthe wombat migration finished on windows\r\n'),
  file('rollout_summaries/summary.md', 'thread_id: main\nupdated_at: ignored\n\n# Deployed the trigram index\n\nRollout context: korean trigram deployment succeeded.\n'),
];
for (const q of ['trigram sidecar', 'wombat migration', '한글 검색', '한글 없는단어조합', '', ' \ufeff\t', 'absent']) add('basic-' + cases.length, basic, q, {}, { memoryDB: true });
add('missing-root-db', [], 'anything');
add('missing-db', [file('MEMORY.md', 'anything')], 'anything');
const spans = [
  file('MEMORY.md', '# Memory Handbook\n\nNo groups consolidated yet.\n'),
  file('windows-span.md', '# Memory Handbook\r\n\r\nNo groups consolidated yet.\r\n'),
  file('later-span.md', 'Leading preamble.\n\n# Memory Handbook\n\nNo groups consolidated yet.\n'),
];
for (const q of ['Handbook consolidated', 'Handbook', 'Handbook missingtokenxyz']) add('span-' + q, spans, q);
add('same-paragraph-line', [file('MEMORY.md', '# Title\n\n## Task 1: ship the trigram sidecar index\n')], 'trigram sidecar');
const ranked = [
  file('memory_summary.md', 'zebra ranking checkpoint\n', now - 90 * day),
  file('MEMORY.md', 'zebra ranking checkpoint\n', now - 90 * day),
  file('rollout_summaries/fresh.md', 'zebra ranking checkpoint\n', now - 7200000),
  file('rollout_summaries/stale.md', 'zebra ranking checkpoint\n', now - 60 * day),
];
add('kind-recency-ranking', ranked, 'zebra ranking');
add('days-filter', ranked, 'zebra ranking', { days: 7 });
add('cutoff-equality', [file('old.md', 'zebra ranking', now - day)], 'zebra ranking', { days: 1 });
add('zero-cutoff-kept', [file('old.md', 'cutoff zebra', -day)], 'cutoff zebra', { nowMs: day, days: 1 });
for (const limit of [-1, 0, 1.5, 3, 20]) add('limit-' + limit, ranked, 'zebra ranking', { limit });
add('per-file-cap', [file('MEMORY.md', 'zebra\n\nzebra zebra\n\nzebra zebra zebra\n\nzebra zebra zebra zebra')], 'zebra');
add('any-mode', [file('one.md', 'alpha'), file('two.md', 'beta')], 'alpha beta', { any: true });
for (const synonyms of [true, false]) {
  add('synonym-' + synonyms, [file('one.md', 'deployment completed')], '배포', { synonyms });
  add('korean-stem-' + synonyms, [file('one.md', '첫 배포 이후 인덱스 재생성이 필요했다.')], '배포까지', { synonyms });
  add('long-query-' + synonyms, [file('one.md', '로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인. bun link healthz 10100.')], '지난번 로컬 소스를 실제 서비스에 연결하고 정상 동작까지 확인한 방법', { synonyms });
}
add('boundary-retry', [file('one.md', 'shipped PR3956 today')], '3956');
add('boundary-presence-retained', [file('one.md', 'NaiControlsPanel PR3956'), file('two.md', 'the LSP server')], '3956 LSP');
add('boundary-no-retry', [file('one.md', 'the LSP server')], 'LSP');
add('stopword-relax-threshold', [file('one.md', 'alpha beta gamma delta')], 'alpha beta gamma delta epsilon zeta theta omega 문제', { synonyms: false });
add('hidden-and-extension', [file('.hidden.md', 'zebra'), file('.hidden/a.md', 'zebra'), file('ignore.MD', 'zebra'), file('nested/a.md', 'zebra')], 'zebra');
const scoped = [
  file('rollout_summaries/here.md', 'thread_id: here\ncwd: /proj/here\n\n# Rollout\n\nThe wombat pipeline shipped.\n'),
  file('rollout_summaries/there.md', 'thread_id: there\ncwd: /proj/here-adjacent\n\n# Rollout\n\nThe wombat pipeline shipped.\n'),
];
const threads = [{ id: 'here', cwd: '/proj/here', origin: null }, { id: 'there', cwd: '/proj/here-adjacent', origin: null }];
for (const [name, options] of [
  ['plain', {}], ['boost', { cwd: '/proj/here' }], ['only', { cwd: '/proj/here', cwdOnly: true }],
  ['adjacent', { cwd: '/proj/here-adjacent', cwdOnly: true }], ['empty', { cwd: '/unrelated', cwdOnly: true }],
  ['unrelated-boost', { cwd: '/unrelated' }], ['backslashes', { cwd: '\\proj\\here', cwdOnly: true }],
  ['blank', { cwd: ' \ufeff\t', cwdOnly: true }],
]) add('cwd-' + name, scoped, 'wombat', options, { threads, legacy: true });
add('prose-half-boost', [file('MEMORY.md', '# Notes\n\napplies_to: cwd=/proj/here; the wombat pipeline is owned here.\n')], 'wombat', { cwd: '/proj/here', cwdOnly: true });
add('prose-adjacent-kept', [file('MEMORY.md', 'wombat lives under /proj/here-adjacent')], 'wombat', { cwd: '/proj/here', cwdOnly: true });
add('no-span-after-dropped-paragraph', [file('MEMORY.md', 'cwd=/proj/here\n\nwombat migration')], 'wombat migration', { cwd: '/proj/here', cwdOnly: true });
const originFiles = [file('rollout_summaries/main.md', 'thread_id: main\ncwd: /proj/main\n\nquokka release'), file('rollout_summaries/other.md', 'thread_id: other\ncwd: /proj/other\n\nquokka release')];
const originRows = [{ id: 'main', cwd: '/proj/main', origin: 'https://github.com/example/alpha.git' }, { id: 'other', cwd: '/proj/other', origin: 'git@github.com:example/beta.git' }];
add('same-origin', originFiles, 'quokka', { cwd: '/worktrees/task', cwdOnly: true }, { threads: originRows, origin: 'git@github.com:example/alpha.git' });
add('no-origin-filter', originFiles, 'quokka', { cwd: '/worktrees/task', cwdOnly: true }, { threads: originRows });
add('frontmatter-beats-thread', [file('s.md', 'thread_id: here\ncwd: /proj/other\n\nnumbat')], 'numbat', { cwd: '/proj/here', cwdOnly: true }, { threads });
add('thread-cwd-join', [file('s.md', 'thread_id: here\n\nnumbat')], 'numbat', { cwd: '/proj/here', cwdOnly: true }, { threads });
add('empty-thread-cwd', [file('s.md', 'thread_id: here\n\nnumbat')], 'numbat', { cwd: '/elsewhere' }, { threads: [{ id: 'here', cwd: '', origin: null }] });
add('aggregate-null-cwd', [file('raw_memories.md', '# Raw Memories\n\n## Thread one\ncwd: /proj/here\n\nplatypus work here.\n\n## Thread two\ncwd: /proj/other\n\nplatypus work elsewhere.')], 'platypus');
add('leading-frontmatter', [file('s.md', 'thread_id: t1\nrollout_path: ignored\ncwd: /proj/here\n\n# Notes\n\ncwd: /proj/other numbat')], 'numbat');
for (const cwd of ['C:\\proj\\here', 'C:/proj/here', 'c:/proj/here', 'C:\\proj\\here\\', 'C:\\proj\\here2']) add('drive-' + cases.length, [file('s.md', 'thread_id: win\ncwd: C:\\proj\\here\\sub\n\nnumbat')], 'numbat', { cwd, cwdOnly: true });
add('extended-windows', [file('s.md', 'thread_id: win\ncwd: \\\\?\\C:\\proj\\here\n\nnumbat')], 'numbat', { cwd: 'C:\\proj\\here', cwdOnly: true });
add('relative-process-cwd', [file('s.md', 'cwd: $WORK/sub\n\nnumbat')], 'numbat', { cwd: '.', cwdOnly: true }, { relative: true });
add('root-is-file', [], 'anything', {}, { rootFile: true });
add('home-is-file', [], 'anything', {}, { homeFile: true });
const oldCwd = process.cwd();
for (let i = 0; i < cases.length; i++) {
  const c = cases[i], home = path.join(root, 'case-' + i), work = path.join(root, 'work-' + i);
  fs.mkdirSync(work); process.chdir(work);
  if (c.homeFile) fs.writeFileSync(home, 'not a directory'); else fs.mkdirSync(home);
  if (c.rootFile) fs.writeFileSync(path.join(home, 'memories'), 'not a directory');
  for (const f of c.files) {
    const p = path.join(home, 'memories', f.path); fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, f.content.replaceAll('$WORK', work)); fs.utimesSync(p, new Date(f.mtime), new Date(f.mtime));
  }
  if (c.memoryDB) { const db = new DatabaseSync(path.join(home, 'memories_1.sqlite')); db.exec('CREATE TABLE stage1_outputs (thread_id TEXT PRIMARY KEY, source_updated_at INTEGER NOT NULL, raw_memory TEXT NOT NULL, rollout_summary TEXT NOT NULL)'); db.close(); }
  if (c.threads) {
    const db = new DatabaseSync(path.join(home, 'state_1.sqlite'));
    db.exec("CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', git_branch TEXT, " + (c.legacy ? '' : 'git_origin_url TEXT, ') + 'updated_at_ms INTEGER)');
    const stmt = db.prepare('INSERT INTO threads VALUES (' + (c.legacy ? '?,?,?,?,?' : '?,?,?,?,?,?') + ')');
    for (const t of c.threads) stmt.run(...(c.legacy ? [t.id, '', t.cwd, null, 0] : [t.id, '', t.cwd, null, t.origin, 0]));
    db.close();
  }
  try {
    const result = searchMemory(c.query, { ...c.options, home, readOriginUrl: () => c.origin ?? null });
    delete result.elapsedMs;
    c.out = JSON.parse(JSON.stringify(result).replaceAll(work, '$WORK').replaceAll(home, '$HOME'));
  } catch { c.error = true; }
}
process.chdir(oldCwd);
process.stdout.write(JSON.stringify(cases, null, 2) + '\n');
