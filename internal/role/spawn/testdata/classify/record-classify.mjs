#!/usr/bin/env node
// record-classify.mjs - records the CXC v0.2.40 answers for the CRW-367 spawn-classifier port.
//
// Usage: node record-classify.mjs <oracle-root> <scratch> <output>
//
// Read-only against the oracle tree. It slices the oracle source, strips the TypeScript types,
// evaluates the classifier functions with new Function, runs the upstream test callbacks that
// call only these functions, drives the direct case tables, applies the CRW rename to inputs and
// expectations, and writes one JSON fixture. Node runs here only as the recorder; the Go test
// replays the file with no Node at run time.
import fs from 'node:fs';
import path from 'node:path';
import assert from 'node:assert/strict';
import { stripTypeScriptTypes } from 'node:module';

const [oracle, scratch, output] = process.argv.slice(2);
if (!oracle || !scratch || !output) {
  console.error('usage: record-classify.mjs <oracle-root> <scratch> <output>');
  process.exit(2);
}
const srcDir = path.join(oracle, 'plugins/codexclaw/components/subagent-config/src');
const testDir = path.join(oracle, 'plugins/codexclaw/components/subagent-config/test');
const source = fs.readFileSync(path.join(srcDir, 'spawn-attach-hook.ts'), 'utf8');
const hookTests = fs.readFileSync(path.join(testDir, 'spawn-attach-hook.test.ts'), 'utf8');
const explorerTests = fs.readFileSync(path.join(testDir, 'explorer-role-routing.test.ts'), 'utf8');

// Three contiguous slices carry exactly this issue's helpers and the constants they use.
const between = (text, start, end) => {
  const from = text.indexOf(start);
  const to = text.indexOf(end);
  assert.ok(from >= 0 && to > from, 'slice markers not found: ' + start);
  return text.slice(from, to);
};
const pieces = [
  between(source, 'export const SUBSPAWN_TOKEN', 'const SUBSPAWN_GRANT_TTL_MS'),
  between(source, 'function stripControlMarkers', 'function dispatchSources'),
  between(source, 'function denyEnvelope', 'export const INLINE_SKILL_OPEN'),
];
const names = ['SUBSPAWN_TOKEN', 'SUBSPAWN_GRANT_RE', 'stripControlMarkers', 'denyEnvelope',
  'RECURSE_DENY_REASON', 'REVIEW_KEYWORDS', 'inferRole', 'isFullHistoryFork', 'isV2SpawnInput',
  'isFernetTokenShape', 'isSpawnToolName', 'isCollaborationToolName'];
const code = stripTypeScriptTypes(pieces.map(piece => piece.replaceAll('export ', '')).join('\n'));
const api = new Function(code + '\nreturn {' + names.join(',') + '};')();

// The CRW rename for this unit's text: R6/R7/R9 and R32, plus the case-preserving mixed-case
// pair the table's regexes do not cover. undefined is recorded as null; the oracle treats both
// the same in every comparison it makes.
const rename = s => s
  .replaceAll('[cxc-subspawn-grant:', '[crw-subspawn-grant:')
  .replaceAll('[CXC-SUBSPAWN-GRANT:', '[CRW-SUBSPAWN-GRANT:')
  .replaceAll('[CxC-SuBsPaWn-GrAnT:', '[CrW-SuBsPaWn-GrAnT:')
  .replaceAll('CXC-SUBSPAWN-ALLOWED', 'CRW-SUBSPAWN-ALLOWED')
  .replaceAll('CXC-ROLE:', 'CRW-ROLE:')
  .replace(/\bcodexclaw\b/g, 'crw');
const renameValue = v => {
  if (typeof v === 'string') return rename(v);
  if (Array.isArray(v)) return v.map(renameValue);
  if (v && typeof v === 'object') {
    const out = {};
    for (const [k, x] of Object.entries(v)) out[k] = renameValue(x);
    return out;
  }
  return v === undefined ? null : v;
};

const sections = [];
const section = fn => {
  const sec = { fn, cases: [] };
  sections.push(sec);
  return sec;
};
let currentTest = '';
const record = (sec, input, oracleValue, options = {}) => {
  const renamedInput = options.goInput !== undefined ? options.goInput : renameValue(input);
  const expected = options.goExpected !== undefined ? options.goExpected : renameValue(oracleValue);
  const same = JSON.stringify(renamedInput) === JSON.stringify(renameValue(input)) &&
    JSON.stringify(expected) === JSON.stringify(oracleValue);
  sec.cases.push({
    input: renamedInput === undefined ? null : renamedInput,
    oracle: oracleValue === undefined ? null : oracleValue,
    expected: expected === undefined ? null : expected,
    classification: same ? 'identical' : 'intentionally-changed',
    reason: same ? '' : 'CRW marker and namespace substitution (R6/R7/R9/R32).',
    test: currentTest,
  });
};

const token = api.SUBSPAWN_TOKEN;
const hex = 'a1b2c3d4'.repeat(8);
const hexUpper = hex.toUpperCase();
const grant = '[CXC-SUBSPAWN-GRANT:' + hex + ']';

// 1. The exported token.
{
  const sec = section('SubspawnToken');
  record(sec, null, token);
}

// 2. stripControlMarkers in both whitespace modes.
{
  const sec = section('StripControlMarkers');
  const cases = [
    [token + ' spawn one helper', false],
    [token + ' spawn one helper', true],
    ['lead ' + token + ' tail', false],
    ['lead ' + token + ' tail', true],
    ['  ' + token + '  ', false],
    ['say cxc-subspawn-allowed instead', false],
    ['say ' + '[cxc-subspawn-grant:' + hex + ']' + ' now', false],
    ['x ' + grant + ' y', false],
    ['x ' + '[CXC-SUBSPAWN-GRANT:' + hexUpper + ']' + ' y', false],
    ['x ' + '[CxC-SuBsPaWn-GrAnT:' + hex + ']' + ' y', false],
    ['x ' + '[CXC-SUBSPAWN-GRANT:' + hex.slice(1) + ']' + ' y', false],
    [grant, true],
    [grant + ' and ' + token, true],
    ['a\n\n\n\nb', false],
    ['a\n\n\n\nb', true],
    ['\uFEFF  keep  \uFEFF', false],
    ['\u0085keep\u0085', false],
    ['\u2028keep\u2029', false],
    ['a\r\n\r\n\r\n\r\nb', false],
    [grant + '  ' + token + ' ', false],
  ];
  for (const [message, preserveWhitespace] of cases) {
    record(sec, { message, preserveWhitespace },
      api.stripControlMarkers(message, preserveWhitespace), { goInput: { message: rename(message), preserveWhitespace } });
  }
  // The oracle's /i does not fold U+017F with s; the CRW counterpart keeps the same spelling so
  // the Go pattern must leave it alone too (a (?i) pattern would fold it and strip the marker).
  const longS = '[CXC-ſUBSPAWN-GRANT:' + hex + ']';
  const longSGo = '[CRW-ſUBSPAWN-GRANT:' + hex + ']';
  record(sec, { message: 'x ' + longS + ' y', preserveWhitespace: false },
    api.stripControlMarkers('x ' + longS + ' y', false),
    { goInput: { message: 'x ' + longSGo + ' y', preserveWhitespace: false }, goExpected: 'x ' + longSGo + ' y' });
}

// 3. denyEnvelope over the escaping surface.
{
  const sec = section('DenyEnvelope');
  const reasons = [
    'plain reason',
    '',
    'quote " and backslash \\ inside',
    'tab\tnewline\ncarriage\rbackspace\bformfeed\f',
    'control \u0001 and \u001f',
    'line separators \u2028 and \u2029',
    '\ud55c\uad6d\uc5b4 \uc774\uc720\uc640 \uc774\ubaa8\uc9c0 \ud83d\ude42',
    'literal \\u2028 text must stay text',
    token + ' must stay literal in the reason',
    api.RECURSE_DENY_REASON,
  ];
  for (const reason of reasons) record(sec, { reason }, api.denyEnvelope(reason));
}

// 4. The recursion deny reason constant.
{
  const sec = section('RecurseDenyReason');
  record(sec, null, api.RECURSE_DENY_REASON);
}

// 5. inferRole: direct cases plus the upstream callbacks that call only these functions.
{
  const sec = section('InferRole');
  const cases = [
    ['worker', 'review this'],
    ['executor', 'map the codebase'],
    ['architect', 'verify interfaces'],
    ['reviewer', 'map the codebase'],
    ['explorer', 'audit the plan for blockers'],
    ['explorer', '\ucf54\ub4dc \uac80\uc99d \ubd80\ud0c1'],
    [undefined, 'map the codebase'],
    [null, 'audit the patch'],
    [null, 'locate the owner'],
    [null, 'preview the diff'],
    [null, 'REVIEW the plan'],
    [null, 'REV\u0130EW'],
    [7, 'review the plan'],
    [true, 'audit the change'],
    [{}, 'verification of the plan'],
    [['review'], 'map the codebase'],
    ['explorer', 'CXC-ROLE: architect\n\nTASK: find the audit logger'],
    [undefined, 'CXC-ROLE: reviewer\n\nTASK: find the audit logger'],
    ['reviewer', 'CXC-ROLE: explorer\n\nTASK: x'],
    ['executor', 'CXC-ROLE: explorer\n\nTASK: x'],
    ['worker', 'CXC-ROLE: architect\n\nTASK: x'],
    ['explorer', 'TASK: quote\nCXC-ROLE: reviewer'],
    ['explorer', 'CXC-ROLE: executor\n\nTASK: x'],
    ['explorer', 'CXC-ROLE: reviewer \t\nTASK: x'],
    ['explorer', 'CXC-ROLE: architect\r\n\r\nTASK: find the audit logger'],
    ['explorer', 'CXC-ROLE: architect\u2028\u2028TASK: x'],
    [null, 'CXC-ROLE: architect'],
    [null, 'CXC-ROLE: architectX\nTASK: x'],
    [null, 'CXC-ROLE:architect\nTASK: x'],
    [null, 'TASK: review the plan'],
    [null, 'audit only\nTASK: x'],
    ['explorer', 'note\nCXC-ROLE: reviewer\nTASK: y'],
    ['explorer', 'CXC-ROLE: reviewer\nTASK: y\nCXC-ROLE: architect'],
  ];
  for (const [agentType, message] of cases) {
    record(sec, { agentType: agentType === undefined ? null : agentType, message },
      api.inferRole(agentType, message));
  }
}

// 6. isV2SpawnInput.
{
  const sec = section('IsV2SpawnInput');
  const cases = [
    {}, { message: 'x' }, { fork_context: true }, { task_name: 't' }, { fork_turns: 'none' },
    { fork_turns: null }, { task_name: 't', fork_turns: 'all' }, { items: [] },
  ];
  for (const toolInput of cases) record(sec, { toolInput }, api.isV2SpawnInput(toolInput));
}

// 7. isFullHistoryFork.
{
  const sec = section('IsFullHistoryFork');
  const cases = [
    { fork_context: true },
    { fork_context: false },
    { fork_context: 1 },
    { fork_context: 'true' },
    { message: 'x' },
    { task_name: 't' },
    { task_name: 't', fork_turns: 'all' },
    { task_name: 't', fork_turns: 'ALL' },
    { task_name: 't', fork_turns: '  ' },
    { task_name: 't', fork_turns: '\u00A0' },
    { task_name: 't', fork_turns: '\uFEFF' },
    { task_name: 't', fork_turns: '\uFEFFall' },
    { task_name: 't', fork_turns: '\u0085' },
    { task_name: 't', fork_turns: ' none ' },
    { task_name: 't', fork_turns: 'none' },
    { task_name: 't', fork_turns: '3' },
    { task_name: 't', fork_turns: '0' },
    { task_name: 't', fork_turns: 3 },
    { task_name: 't', fork_turns: 0 },
    { task_name: 't', fork_turns: null },
    { task_name: 't', fork_turns: true },
    { task_name: 't', fork_turns: ['all'] },
    { task_name: 't', fork_turns: { mode: 'all' } },
    { fork_turns: 'all' },
    { task_name: 't', fork_turns: 'all', fork_context: false },
  ];
  for (const toolInput of cases) record(sec, { toolInput }, api.isFullHistoryFork(toolInput));
}

// 8. isFernetTokenShape over the upstream vectors and the boundaries the issue names.
{
  const sec = section('IsFernetTokenShape');
  const FERNET_VECTOR = 'gAAAAAAdwJ6wAAECAwQFBgcICQoLDA0ODy021cpGVWKZ_eEwCGM4BLLF_5CV9dOPmrhuVUPgJobwOz7JcbmrR64jVmpU4IwqDA==';
  const core = FERNET_VECTOR.slice(0, -2);
  const frame = options => Buffer.concat([
    Buffer.from([options.version ?? 0x80]),
    options.timestamp ?? Buffer.alloc(8, 0),
    Buffer.alloc(16, 0x11),
    Buffer.alloc(options.ciphertextBytes ?? 16, 0x22),
    Buffer.alloc(32, 0x33),
  ]);
  const wire = (buf, padded = true) => {
    const unpadded = Buffer.from(buf).toString('base64url');
    return padded ? unpadded + '='.repeat((4 - unpadded.length % 4) % 4) : unpadded;
  };
  const cases = [
    ['reference padded', FERNET_VECTOR],
    ['reference unpadded', core],
    ['n=1 padded', wire(frame({ ciphertextBytes: 16 }))],
    ['n=1 unpadded', wire(frame({ ciphertextBytes: 16 }), false)],
    ['n=2 padded', wire(frame({ ciphertextBytes: 32 }))],
    ['n=3 padded', wire(frame({ ciphertextBytes: 48 }))],
    ['n=3 unpadded', wire(frame({ ciphertextBytes: 48 }), false)],
    ['non-gAAAA timestamp', wire(frame({ timestamp: Buffer.alloc(8, 0xff) }))],
    ['wrong version byte', wire(frame({ version: 0x81 }))],
    ['truncated frame', FERNET_VECTOR.slice(0, 80)],
    ['empty ciphertext', wire(frame({ ciphertextBytes: 0 }))],
    ['non-16-byte-block ciphertext', wire(frame({ ciphertextBytes: 24 }))],
    ['short gAAAA prefix', 'gAAAAx'],
    ['old e2e fixture', 'gAAAAABopaque-payload'],
    ['impossible base64 length', 'gAAAA'],
    ['old invalid fixture', 'gAAAAAB' + 'aB9_'.repeat(30) + '=='],
    ['embedded whitespace', core.slice(0, 40) + ' ' + core.slice(40) + '=='],
    ['standard-alphabet characters', FERNET_VECTOR.replaceAll('_', '/')],
    ['partial padding', core + '='],
    ['excess padding', FERNET_VECTOR + '='],
    ['trailing newline', FERNET_VECTOR + '\n'],
    ['padding with carriage return', core + '=\r'],
    ['mid-string padding', core.slice(0, 20) + '=' + core.slice(20) + '=='],
    ['nonzero unused pad bits', core.slice(0, -1) + 'B=='],
    ['only padding', '='],
    ['empty', ''],
    ['non-ASCII', '\u00e9' + core],
  ];
  for (const [label, tokenText] of cases) {
    const value = api.isFernetTokenShape(tokenText);
    record(sec, { token: tokenText, label }, value);
  }
}

// 9. Tool-name classification.
{
  const spawnSec = section('IsSpawnToolName');
  const collabSec = section('IsCollaborationToolName');
  const cases = ['spawn_agent', 'collaborationspawn_agent', 'collaboration.spawn_agent',
    'collaboration_spawn_agent', 'shell', 'multi_agent_v1.spawn_agent', 'Spawn_agent',
    ' spawn_agent', 'collaboration.spawn_agent ', 'collaborationspawnagent', '', null, 7,
    ['spawn_agent'], { name: 'spawn_agent' }, true];
  for (const name of cases) {
    record(spawnSec, { name }, api.isSpawnToolName(name));
    record(collabSec, { name }, api.isCollaborationToolName(name));
  }
}

// 10. Replay the upstream callbacks that call only these functions and record every call.
const inferSec = sections.find(s => s.fn === 'InferRole');
const v2Sec = sections.find(s => s.fn === 'IsV2SpawnInput');
const forkSec = sections.find(s => s.fn === 'IsFullHistoryFork');
const spawnSec = sections.find(s => s.fn === 'IsSpawnToolName');
const collabSec = sections.find(s => s.fn === 'IsCollaborationToolName');
const wrapped = {
  inferRole: (agentType, message) => {
    const value = api.inferRole(agentType, message);
    record(inferSec, { agentType: agentType === undefined ? null : agentType, message }, value);
    return value;
  },
  isV2SpawnInput: toolInput => {
    const value = api.isV2SpawnInput(toolInput);
    record(v2Sec, { toolInput }, value);
    return value;
  },
  isFullHistoryFork: toolInput => {
    const value = api.isFullHistoryFork(toolInput);
    record(forkSec, { toolInput }, value);
    return value;
  },
  isSpawnToolName: name => {
    const value = api.isSpawnToolName(name);
    record(spawnSec, { name: name === undefined ? null : name }, value);
    return value;
  },
  isCollaborationToolName: name => {
    const value = api.isCollaborationToolName(name);
    record(collabSec, { name: name === undefined ? null : name }, value);
    return value;
  },
};
const selected = [
  'explicit explorer ignores task vocabulary, including negative review instructions',
  'legacy read-only headers remain deliberate role selections',
  'inferRole: worker -> executor; review keywords -> reviewer; default explorer',
  'isV2SpawnInput and isFullHistoryFork classify spawn shapes',
  'isSpawnToolName / isCollaborationToolName accept native V2 hook names',
  'architect role identity wins review words while explicit write/reviewer roles win markers',
];
let replayed = 0;
for (const text of [explorerTests, hookTests]) {
  for (const match of text.matchAll(/test\((?:'|")([^'"\n]+)(?:'|"), \(\) => \{([\s\S]*?)\n\}\);/g)) {
    const name = match[1];
    if (!selected.includes(name)) continue;
    currentTest = name;
    const block = stripTypeScriptTypes('test(' + JSON.stringify(name) + ', () => {' + match[2] + '\n});');
    new Function('test', 'assert', ...Object.keys(wrapped), block)(
      (_, fn) => fn(), assert, ...Object.values(wrapped));
    currentTest = '';
    replayed++;
  }
}
assert.equal(replayed, selected.length, 'not every selected upstream callback was found');

fs.mkdirSync(scratch, { recursive: true });
fs.mkdirSync(path.dirname(output), { recursive: true });
fs.writeFileSync(output, JSON.stringify({
  oracle: 'CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d)',
  source: 'subagent-config/src/spawn-attach-hook.ts:279-280,409-419,450-595',
  tests: 'subagent-config/test/spawn-attach-hook.test.ts; subagent-config/test/explorer-role-routing.test.ts',
  note: 'Recorded by record-classify.mjs under Node 24. input is the CRW-named case the Go test replays; ' +
    'oracle is the raw answer the CXC source gave for the CXC-named case; expected is the renamed answer.',
  sections,
}, null, 2) + '\n');
console.log('recorded ' + sections.map(s => s.fn + ':' + s.cases.length).join(' '));
console.log('upstream callbacks replayed: ' + replayed);
