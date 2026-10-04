// Pinned CXC v0.2.40 oracle. Every database is synthesized under a temporary home.
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
const dist = process.argv[2];
const m = await import(dist + '/memory-requeue.js');
const { openDbReadWrite, openDbReadOnly } = await import(dist + '/sqlite.js');
const schema = 'CREATE TABLE jobs (kind TEXT, job_key TEXT, status TEXT, retry_remaining INTEGER, retry_at INTEGER, last_error TEXT, input_watermark INTEGER, last_success_watermark INTEGER)';
const loose = 'CREATE TABLE jobs (kind, job_key, status, retry_remaining, retry_at, last_error, input_watermark, last_success_watermark)';
const quoted = x => x === null ? 'NULL' : typeof x === 'number' ? String(x) : "'" + x.replaceAll("'", "''") + "'";
const row = (key, error, kind = 'memory_stage1', status = 'error', retries = 0) => 'INSERT INTO jobs VALUES (' + [kind, key, status, retries, 999, error, 10, 5].map(quoted).join(',') + ')';
const pair = [schema, row('a', 'stream closed early'), row('b', 'context window exceeded')];
const cases = [
  { id: 'dry', sql: pair },
  { id: 'apply', sql: [schema, row('a', '429 rate limit')], options: {apply: true, retries: 2} },
  { id: 'context-default', sql: [schema, row('a', 'context window exceeded')] },
  { id: 'context-opt-in', sql: pair, options: {includeContextWindow: true} },
  { id: 'non-exhausted', sql: [schema, row('done', null, 'memory_stage1', 'done'), row('running', null, 'memory_stage1', 'running'), row('retryable', 'capacity', 'memory_stage1', 'error', 2), row('target', 'capacity')], options: {apply: true} },
  { id: 'kind', sql: [schema, row('a', 'capacity'), row('b', 'capacity'), row('c', 'capacity', 'memory_consolidate_global')], options: {kind: 'memory_stage1'} },
  { id: 'consolidation-default', sql: [schema, row('a', 'capacity', 'memory_consolidate_global')], options: {apply: true} },
  { id: 'unsupported', sql: ['CREATE TABLE jobs (kind, status)'], options: {apply: true} },
  { id: 'missing', options: {apply: true} },
  { id: 'empty', sql: [schema], options: {apply: true} },
  { id: 'no-jobs', sql: ['CREATE TABLE other (x)'], options: {apply: true} },
  { id: 'corrupt', mode: 'corrupt', options: {apply: true} },
  { id: 'directory', mode: 'directory', options: {apply: true} },
  { id: 'newest', sql: [schema, row('old', 'capacity')], newer: [schema, row('new', 'incomplete response')], options: {apply: true} },
  { id: 'untyped', sql: [loose, row('a', 'capacity')], options: {apply: true} },
  { id: 'coercion', sql: [loose, row(null, 'capacity', null), row(12, 429, 12), "INSERT INTO jobs VALUES (x'61',x'62','error',0,999,x'6363',10,5)"], options: {apply: true} },
  { id: 'null-kind', sql: [loose, row('a', 'capacity', null)], options: {apply: true, kind: 'null'} },
  { id: 'all-causes', sql: [schema, ...['capacity', 'incomplete response', 'stream closed', null, 'other', 'context window exceeded', 'capacity'].map((e,i) => row(String(i),e))] },
  { id: 'missing-backoff', sql: ['CREATE TABLE jobs (kind, job_key, status, retry_remaining, last_error)', "INSERT INTO jobs VALUES ('stage','a','error',0,'capacity')"], options: {apply: true} },
  { id: 'duplicate-zero-retry', sql: [schema, row('a','capacity'), row('a','capacity')], options: {apply: true, retries: 0.5} },
  { id: 'update-rollback', sql: [schema, row('a','capacity'), row('b','capacity'), "CREATE TRIGGER abort_second BEFORE UPDATE ON jobs WHEN old.job_key='b' BEGIN SELECT RAISE(ABORT,'second update refused'); END"], options: {apply: true} },
  { id: 'running-predicate', sql: [schema, row('a','capacity'), row('b','capacity'), "CREATE TRIGGER pick_up AFTER UPDATE ON jobs WHEN old.job_key='a' BEGIN UPDATE jobs SET status='running' WHERE job_key='b'; END"], options: {apply: true} },
  { id: 'commit-rollback', sql: [schema, row('a','capacity'), 'CREATE TABLE parent (id PRIMARY KEY)', 'CREATE TABLE child (id REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)', 'CREATE TRIGGER deferred_error AFTER UPDATE ON jobs BEGIN INSERT INTO child VALUES (99); END'], options: {apply: true} },
];
for (const value of [0, -1, 0.5, 2.7, 'NaN', 'Infinity', '-Infinity', 1e20, 1e21]) cases.push({id: 'retries-' + value, sql: [schema, row('a', 'capacity')], options: {apply: true, retries: value}});
for (const value of [0, -1, 1, 1.7, 'NaN', 'Infinity', 1e300]) cases.push({id: 'limit-' + value, sql: [schema, row('a','capacity'), row('b','other'), row('c','context window exceeded')], options: {apply: true, limit: value}});
for (const missing of ['kind','job_key','status','retry_remaining','last_error']) cases.push({id: 'missing-' + missing, sql: ['CREATE TABLE jobs (' + ['kind','job_key','status','retry_remaining','last_error'].filter(x=>x!==missing).join(',') + ')'], options: {apply:true}});
const out = [];
for (const c of cases) {
  const home = mkdtempSync(join(tmpdir(), 'crw-requeue-'));
  try {
    for (const [name, sql] of [['memories_1.sqlite', c.sql], ['memories_2.sqlite', c.newer]]) {
      if (!sql) continue;
      const db = openDbReadWrite(join(home,name));
      try { for (const statement of sql) db.exec(statement); } finally { db.close(); }
    }
    if (c.mode === 'corrupt') writeFileSync(join(home,'memories_1.sqlite'),'this is not a database');
    if (c.mode === 'directory') mkdirSync(join(home,'memories_1.sqlite'));
    const options = {...c.options};
    for (const k of ['retries','limit']) if (typeof options[k] === 'string') options[k] = Number(options[k]);
    const result = m.requeueExhaustedMemoryJobs(home, options);
    let snapshotSQL = '', rows = [];
    if (c.sql || c.newer) {
      const db = openDbReadOnly(join(home,c.newer ? 'memories_2.sqlite' : 'memories_1.sqlite'));
      try {
        const cols = db.prepare('PRAGMA table_info(jobs)').all();
        if (cols.length) {
          snapshotSQL = cols.some(x=>x.name==='retry_remaining') ? 'SELECT *, typeof(retry_remaining) AS retry_type FROM jobs ORDER BY rowid' : 'SELECT * FROM jobs ORDER BY rowid';
          rows = db.prepare(snapshotSQL).all();
        }
      } finally { db.close(); }
    }
    const clean = x => JSON.parse(JSON.stringify(x).replaceAll(home,'<HOME>'));
    out.push({...c, result:clean(result), text:clean(m.formatRequeue(result)), snapshotSQL, rows:clean(rows)});
  } finally { rmSync(home, {recursive:true, force:true}); }
}
process.stdout.write(JSON.stringify({transient:[...m.TRANSIENT_CAUSES],cases:out},null,2) + '\n');
