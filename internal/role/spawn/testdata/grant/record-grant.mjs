import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { stripTypeScriptTypes } from 'node:module';
// usage: node record-grant.mjs <oracle checkout> <scratch dir> <output.json>
// Placeholders in the recorded text: {N1}.. a minted nonce ({N1:upper}, {N1:short}, {N1:long} variants),
// {LONE} a lone UTF-16 surrogate, {UID} the numeric uid.
const [oracle, scratch, output] = process.argv.slice(2);
const source = fs.readFileSync(path.join(oracle, 'plugins/codexclaw/components/subagent-config/src/spawn-attach-hook.ts'), 'utf8');
const slice = source.slice(source.indexOf('export const SUBSPAWN_TOKEN'), source.indexOf('/** Dedupe marker for the leaf guard block. */')).replace('export const', 'const')
  + source.slice(source.indexOf('/** True when the hook stdin identifies'), source.indexOf('function stripControlMarkers'));
const BASE = 1_800_000_000_000;
let clock = BASE, tmp = '', seed = 0;
const proc = { getuid: process.getuid, pid: process.pid, cwd: () => process.cwd() };
const o = new Function('createHash', 'randomBytes', 'resolve', 'tmpdir', 'mkdirSync', 'writeFileSync', 'renameSync', 'readFileSync', 'rmSync', 'process', 'Date',
  stripTypeScriptTypes(slice) + '\nreturn { SUBSPAWN_TOKEN, SUBSPAWN_GRANT_RE, SUBSPAWN_GRANT_TTL_MS, isSubagentSpawner, grantScope, grantDir, grantPath, mintRecursionGrant, consumeRecursionGrant };')(
  crypto.createHash, n => crypto.createHash('sha256').update('crw-612 recorder nonce ' + ++seed).digest().subarray(0, n), path.resolve, () => tmp, fs.mkdirSync, fs.writeFileSync, fs.renameSync, fs.readFileSync, fs.rmSync, proc, { now: () => clock });
const real = value => JSON.parse(JSON.stringify(value).replaceAll('{LONE}', '\\ud800'));
const rename = text => text.replace(/cxc-subspawn-grant/gi, m => (m.startsWith('cxc') ? 'crw' : 'CRW') + m.slice(3));
const fresh = name => { tmp = fs.mkdtempSync(path.join(scratch, name + '-')); return tmp; };
const expand = (template, nonces) => template.replace(/\{(N\d+)(?::(\w+))?\}/g, (_, key, how) => {
  const n = nonces[key];
  return how === 'upper' ? n.toUpperCase() : how === 'short' ? n.slice(0, 63) : how === 'long' ? n + 'f' : n;
});
const keyDir = obj => o.grantDir(o.grantScope(real(obj)));
const listing = obj => { try { return fs.readdirSync(keyDir(obj)).length; } catch { return 0; } };
fs.mkdirSync(scratch, { recursive: true });

const spawner = [{ agent_id: 'a1' }, { agent_type: 'worker' }, { agent_id: '', agent_type: '' }, { agent_id: 'a1', agent_type: '' }, {}, { agent_id: 5 }, { agent_id: null, agent_type: 't' }, { agent_id: true }, { agent_id: ['x'] }, { agent_id: '', agent_type: 'explorer' }]
  .map(obj => ({ obj, oracle: o.isSubagentSpawner(obj), expected: o.isSubagentSpawner(obj) }));

const A = { cwd: '/ws/a', session_id: 's1' };
tmp = '/rec/tmp';
const paths = [
  [{ cwd: '/ws/a/../b/', session_id: 's1' }, 'n1'], [{ cwd: '/ws//x', session_id: 'unknown-session' }, 'n2'], [{ cwd: '/ws/한글', session_id: '세션-1' }, 'n3'],
  [{ cwd: '/ws/c' }, 'n4'], [{ cwd: '/ws', session_id: 'a{LONE}b' }, 'n5'], [{ cwd: '/ws{LONE}', session_id: 's' }, 'n6'],
].map(([obj, label]) => {
  const scope = o.grantScope(real(obj)), nonce = crypto.createHash('sha256').update(label).digest('hex');
  const dir = o.grantDir(scope), oracleDirName = path.basename(path.dirname(dir)).replace(String(process.getuid()), '{UID}');
  return { obj, nonce, scope: JSON.stringify(obj).includes('{LONE}') ? null : { cwd: scope.cwd, session: scope.session },
    oracleDirName, expectedDirName: oracleDirName.replace('codexclaw-', 'crw-'), scopeDigest: path.basename(dir), file: path.basename(o.grantPath(scope, nonce)),
    classification: 'intentionally-changed', reason: 'CRW directory prefix (codexclaw- to crw-); the key and file digests are identical.' };
});

const m = k => '[CXC-SUBSPAWN-GRANT:{' + k + '}]';
const mint = (as, obj, at = 0) => ({ op: 'mint', as, obj, at });
const consume = (obj, message, at = 0, crw) => ({ op: 'consume', obj, message, at, ...(crw ? { crw } : {}) });
const count = (obj, at = 0) => ({ op: 'count', obj, at });
const sequences = [
  { name: 'a grant is consumed once', steps: [mint('N1', A), consume(A, m('N1'), 1000), consume(A, m('N1'), 2000), count(A)] },
  { name: 'two markers refuse without spending the grant', steps: [mint('N1', A), consume(A, m('N1') + ' ' + m('N1')), consume(A, m('N1') + m('N1')), count(A), consume(A, m('N1')), count(A)] },
  { name: 'marker count and shape', steps: [mint('N1', A), consume(A, 'no marker here'), consume(A, ''), consume(A, '[CXC-SUBSPAWN-GRANT:{N1:short}]'), consume(A, '[CXC-SUBSPAWN-GRANT:{N1:long}]'),
    consume(A, '[CXC-SUBSPAWN-GRANT:{N1}'), consume(A, 'CXC-SUBSPAWN-GRANT:{N1}]'), consume(A, '[CXC-SUBSPAWN-GRANT: {N1}]'), consume(A, '[CXC-SUBSPAWN-ALLOWED]'), count(A), consume(A, 'x\n' + m('N1') + '\ny'), count(A)] },
  { name: 'prefix case folds, nonce case and non-ASCII look-alikes do not', steps: [mint('N1', A), mint('N2', A), mint('N3', A),
    consume(A, '[cxc-subspawn-grant:{N1}]'), consume(A, '[CXC-SubSpawn-Grant:{N2:upper}]'), consume(A, '[CXC-SubSpawn-Grant:{N2}]'),
    consume(A, '[CXC-SUB\u017fPAWN-GRANT:{N3}]', 0, '[CRW-SUB\u017fPAWN-GRANT:{N3}]'), consume(A, m('N3')), count(A)] },
  { name: 'expiry is inclusive at fifteen minutes and a spent grant is removed', steps: [mint('N1', A, 0), consume(A, m('N1'), 900000), mint('N2', A, 0), consume(A, m('N2'), 900001), count(A), consume(A, m('N2'), 0), mint('N3', A, 10), consume(A, m('N3'), 10 + 899999), count(A)] },
  { name: 'cwd and session scope the grant', steps: [mint('N1', A), consume({ cwd: '/ws/b', session_id: 's1' }, m('N1')), consume({ cwd: '/ws/a', session_id: 's2' }, m('N1')), consume({ cwd: '/ws/a' }, m('N1')), consume({ session_id: 's1' }, m('N1')),
    consume({ cwd: '/ws/x/../a/', session_id: 's1' }, m('N1')), count(A)] },
  { name: 'missing, empty and non-string cwd and session fall back to the defaults', steps: [mint('N1', {}), consume({}, m('N1')), mint('N2', { cwd: '', session_id: '' }), consume({}, m('N2')), mint('N3', { cwd: 5, session_id: 7 }), consume({ cwd: null }, m('N3')),
    mint('N4', { session_id: 'unknown-session' }), consume({}, m('N4'))] },
  { name: 'a lone surrogate in the session is hashed as U+FFFD', steps: [mint('N1', { cwd: '/ws', session_id: 'a{LONE}' }), consume({ cwd: '/ws', session_id: 'a\ufffd' }, m('N1')), mint('N2', { cwd: '/ws', session_id: 'a{LONE}' }), consume({ cwd: '/ws', session_id: 'a{LONE}' }, m('N2'))] },
  { name: 'a malformed candidate or an open bracket in front of a good marker still consumes', steps: [mint('N1', A), mint('N2', A), consume(A, '[CXC-SUBSPAWN-GRANT:' + m('N1')), consume(A, '[CXC-SUBSPAWN-GRANT:{N2:short}] ' + m('N2')), count(A)] },
  { name: 'no grant tree at all', steps: [consume({ cwd: '/ws/none', session_id: 'none' }, '[CXC-SUBSPAWN-GRANT:' + 'a'.repeat(64) + ']')] },
];
for (const sequence of sequences) {
  fresh('seq');
  const nonces = {};
  for (const step of sequence.steps) {
    clock = BASE + step.at;
    if (step.op === 'mint') { nonces[step.as] = o.mintRecursionGrant(real(step.obj)); step.oracle = nonces[step.as] !== null; }
    else if (step.op === 'count') step.oracle = listing(step.obj);
    else step.oracle = o.consumeRecursionGrant(real(step.obj), expand(step.message, nonces));
    step.expected = step.oracle;
    if (step.op === 'consume') { step.crw = step.crw ?? rename(step.message); }
  }
  for (const step of sequence.steps) if (step.op === 'consume') { step.message = step.crw; delete step.crw; }
}

const bodies = ['{"expiresAt":1900000000000}', '{"expiresAt":"1900000000000"}', '{"expiresAt":null}', '{}', '[]', '', 'null', '[1]', '"x"', '{"expiresAt":1e999}', '{"expiresAt":-1e999}', '{"expiresAt":1900000000000.5}',
  '{"expiresAt":1799999999999}', '{"expiresAt":1800000000000}', '{"expiresAt":1900000000000} trailing', '\ufeff{"expiresAt":1900000000000}', '{"expiresAt":1900000000000,"expiresAt":1}', '{"expiresAt":1,"expiresAt":1900000000000}',
  '{"expiresAt":true}', '{"expiresAt":[1900000000000]}', '{"expiresAt":1900000000000}\n', '{"expiresAt":1900000000000', '{"expiresAt":1900000000000} {}', '{"expiresAt":1900000000000} true', '{"expiresAt":1900000000000}{"expiresAt":1}'];
fresh('planted');
clock = BASE;
const planted = bodies.map((content, i) => {
  const nonce = crypto.createHash('sha256').update('planted-' + i).digest('hex');
  fs.mkdirSync(keyDir(A), { recursive: true, mode: 0o700 });
  fs.writeFileSync(o.grantPath(o.grantScope(A), nonce), content, { mode: 0o600 });
  const answer = o.consumeRecursionGrant(A, '[CXC-SUBSPAWN-GRANT:' + nonce + ']');
  assert.equal(listing(A), 0, 'the oracle removes the claimed file after every consume');
  return { content, nonce, oracle: answer, expected: answer };
});

fresh('shape');
clock = BASE;
const shapeNonce = o.mintRecursionGrant(A);
const shapeFile = o.grantPath(o.grantScope(A), shapeNonce);
const mode = p => (fs.statSync(p).mode & 0o777).toString(8);
const mintShape = { at: 0, content: fs.readFileSync(shapeFile, 'utf8'), fileMode: mode(shapeFile), keyDirMode: mode(path.dirname(shapeFile)), uidDirMode: mode(path.dirname(path.dirname(shapeFile))), nonceLength: shapeNonce.length };

const setups = {
  'key directory mode 0755': t => { fs.mkdirSync(path.dirname(t.key), { recursive: true, mode: 0o700 }); fs.mkdirSync(t.key, { mode: 0o700 }); fs.chmodSync(t.key, 0o755); },
  'key directory mode 0770': t => { fs.mkdirSync(path.dirname(t.key), { recursive: true, mode: 0o700 }); fs.mkdirSync(t.key, { mode: 0o700 }); fs.chmodSync(t.key, 0o770); },
  'uid directory mode 0755': t => { fs.mkdirSync(t.key, { recursive: true, mode: 0o700 }); fs.chmodSync(path.dirname(t.key), 0o755); },
  'uid directory mode 0770': t => { fs.mkdirSync(t.key, { recursive: true, mode: 0o700 }); fs.chmodSync(path.dirname(t.key), 0o770); },
  'uid directory is a symlink': t => { fs.mkdirSync(path.join(t.root, 'elsewhere', path.basename(t.key)), { recursive: true, mode: 0o700 }); fs.symlinkSync(path.join(t.root, 'elsewhere'), path.dirname(t.key)); },
  'key directory is a symlink': t => { fs.mkdirSync(path.dirname(t.key), { recursive: true, mode: 0o700 }); fs.mkdirSync(path.join(t.root, 'elsewhere'), { mode: 0o700 }); fs.symlinkSync(path.join(t.root, 'elsewhere'), t.key); },
  'uid directory is a plain file': t => { fs.writeFileSync(path.dirname(t.key), 'x'); },
  'key directory is a plain file': t => { fs.mkdirSync(path.dirname(t.key), { recursive: true, mode: 0o700 }); fs.writeFileSync(t.key, 'x'); },
  'uid directory is a FIFO': t => { execFileSync('mkfifo', ['-m', '700', path.dirname(t.key)]); },
  'key directory is a FIFO': t => { fs.mkdirSync(path.dirname(t.key), { recursive: true, mode: 0o700 }); execFileSync('mkfifo', ['-m', '700', t.key]); },
};
const unsafe = Object.entries(setups).map(([name, arrange]) => {
  const root = fresh('unsafe');
  clock = BASE;
  arrange({ root, key: keyDir(A) });
  const nonce = o.mintRecursionGrant(A);
  const consumed = o.consumeRecursionGrant(A, '[CXC-SUBSPAWN-GRANT:' + (nonce ?? 'a'.repeat(64)) + ']');
  const accepted = nonce !== null;
  return { name, oracle: { minted: accepted, consumed }, expected: { minted: false, consumed: false }, classification: accepted ? 'intentionally-changed' : 'identical',
    ...(accepted ? { reason: 'Security fix (port: fixed): the per-uid and key directories are used only when they are real directories owned by the current uid with mode 0700; the oracle mints into and consumes from whatever mkdir recursive finds.' } : {}) };
});

fs.writeFileSync(output, JSON.stringify({ oracle: 'CXC v0.2.40 (3c1459ac)', source: 'subagent-config/src/spawn-attach-hook.ts:279-281,351-408', baseMs: BASE,
  constants: { token: o.SUBSPAWN_TOKEN, markerSource: o.SUBSPAWN_GRANT_RE.source, markerFlags: o.SUBSPAWN_GRANT_RE.flags, ttlMs: o.SUBSPAWN_GRANT_TTL_MS },
  spawner, paths, mintShape, sequences, planted, unsafe }, null, 2) + '\n');
console.log(spawner.length + ' spawner cases, ' + paths.length + ' path cases, ' + sequences.reduce((n, s) => n + s.steps.length, 0) + ' sequence steps, ' + planted.length + ' planted bodies, ' + unsafe.length + ' unsafe-directory cases recorded.');
