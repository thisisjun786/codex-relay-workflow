import fs from 'node:fs';
import path from 'node:path';
import assert from 'node:assert/strict';

// Records the answers of CXC v0.2.40 subagent-config/src/final-gate-guard.ts (commit 3c1459ac) for the cases of
// subagent-config/test/final-gate-guard.test.ts and for each further branch of checkFinalGatePrereqs.
// Usage: node record.mjs <oracle root> <scratch dir> <output file>   (Node is test data only; the Go test never runs it.)
const [oracle, scratch, output] = process.argv.slice(2);
process.env.GIT_CEILING_DIRECTORIES = scratch;
const guard = await import(path.join(oracle, 'plugins/codexclaw/components/subagent-config/src/final-gate-guard.ts'));
const table = JSON.parse(fs.readFileSync(new URL('../../../../../contract/schema/cxc/name-substitution.json', import.meta.url), 'utf8'));
const WS = '$' + '{WS}'; // the placeholder of the case's working directory

// The corpus replayer's name substitution (contract/schema/cxc/name-substitution.json): the regex rules in order, a rule
// with repeat until the text stops changing; the Go template's group references and $$ are expanded by hand.
const expand = (template, m) => template.replace(/\$\$|\$\{(\d+)\}/g, (t, n) => (t === '$$' ? '$' : m[Number(n)] ?? ''));
const rename = text => {
  for (const rule of table.rules) {
    if (rule.kind !== 'regex') continue;
    const re = new RegExp(rule.regex, 'g');
    for (let before = null; before !== text;) {
      before = text;
      text = text.replace(re, (...a) => expand(rule.replace, a.slice(0, a.findIndex(x => typeof x === 'number'))));
    }
  }
  return text;
};

const SESSION = 'sess-1', SLUG = 'demo', PACKET = guard.FINAL_GATE_MARKER + ' please review the final gate';
const HERE = { kind: 'resolved', commitSha: 'aaaaaaa', dirty: false };
const MOVED = { kind: 'resolved', commitSha: 'bbbbbbb', dirty: false };
const NO_GIT = { kind: 'unavailable', commitSha: '', dirty: false };
const SRC = { ...HERE, sourceRoot: '/src/tree' };
const STATE = '.codexclaw/sessions/', PLAN = '.codexclaw/goalplans/' + SLUG + '/goalplan.json', EVID = '.codexclaw/evidence/';

// fixture() builds the three real files the oracle suite builds (session state, goalplan, receipts) and what the check is
// given. A receipt spec is an identity, 'missing', 'empty', 'dir' or raw text; an option named testPath replaces the path
// the plan records.
const fixture = (o = {}) => {
  const files = {}, dirs = [EVID], symlinks = {};
  if (!o.omitSession) files[STATE + SESSION + '.json'] = o.stateRaw ?? JSON.stringify({ sessionId: SESSION, slug: o.slug ?? SLUG, ...o.state });
  const receipt = (name, spec) => {
    if (spec === undefined || spec === 'missing') return undefined;
    if (spec === 'dir') { dirs.push(EVID + name); return EVID + name; }
    files[EVID + name] = spec === 'empty' ? '' : typeof spec === 'string' ? spec : JSON.stringify({ kind: 'test', sourceIdentity: spec });
    return EVID + name;
  };
  const made = [receipt('test.json', o.testReceipt), receipt('qa.json', o.qaReceipt)];
  const testPath = 'testPath' in o ? o.testPath : made[0];
  if (o.link) symlinks[EVID + 'link.json'] = o.link;
  if (!o.omitPlan) {
    const visual = ['web', 'tui', 'desktop'].includes(o.surface);
    files[PLAN] = o.planRaw ?? JSON.stringify({
      objective: 'o', slug: SLUG,
      criteria: o.criteria ?? [{ id: 'c-1', scenario: 's', ...(o.surface ? { surface: o.surface } : {}) }],
      ...(o.omitGate ? {} : { finalGate: 'gate' in o ? o.gate : { status: 'in_flight', qaRequired: visual,
        ...(testPath !== undefined ? { testReceiptPath: testPath } : {}), ...(made[1] !== undefined ? { qaReceiptPath: made[1] } : {}) } }),
    });
  }
  Object.assign(files, o.files);
  return { packet: o.packet ?? PACKET, session: o.session ?? SESSION, capture: 'capture' in o ? o.capture : HERE, files, dirs, symlinks };
};

const NOREC = /missing, empty or unreadable/;
// [name, fixture options, want]: want is [ok, pattern for the reason], the assertion of the upstream test where it has one.
const cases = [
  // final-gate-guard.test.ts, one case per assertion (its two hook tests need the hook leg and are not recorded here)
  ['a spawn without the marker is none of the guard\'s business', { testReceipt: 'missing', packet: 'please review this plan' }, [true]],
  ['a marked spawn with a fresh test receipt and no visual criteria is allowed', { testReceipt: HERE }, [true]],
  ['a marked spawn with no test receipt is denied and names the gap', { testReceipt: 'missing' }, [false, /test receipt path is not recorded/]],
  ['an empty test receipt is denied', { testReceipt: 'empty' }, [false, NOREC]],
  ['a web criterion demands a QA receipt', { surface: 'web', testReceipt: HERE }, [false, /QA receipt path is not recorded/]],
  ['a tui criterion demands a QA receipt', { surface: 'tui', testReceipt: HERE }, [false]],
  ['a desktop criterion demands a QA receipt', { surface: 'desktop', testReceipt: HERE }, [false, /QA receipt path is not recorded/]],
  ['surface logic needs no QA receipt', { surface: 'logic', testReceipt: HERE }, [true]],
  ['surface api is not treated as visual', { surface: 'api', testReceipt: HERE }, [true]],
  ['a web criterion with both receipts fresh is allowed', { surface: 'web', testReceipt: HERE, qaReceipt: HERE }, [true]],
  ['a receipt from a different tree is denied and shows both shas', { testReceipt: MOVED }, [false, /bbbbbbb.*aaaaaaa/s]],
  ['compare: unavailable wins over a sha difference', { testReceipt: MOVED, capture: NO_GIT }, [true]],
  ['compare: a sha mismatch denies', { testReceipt: MOVED }, [false]],
  ['compare: clean against dirty denies', { testReceipt: { ...HERE, dirty: true } }, [false]],
  ['compare: a treeHash mismatch denies', { testReceipt: { ...HERE, dirty: true, treeHash: 'x' }, capture: { ...HERE, dirty: true, treeHash: 'y' } }, [false]],
  ['compare: identical allows', { testReceipt: HERE }, [true]],
  ['no session state fails open', { testReceipt: 'missing', omitSession: true }, [true]],
  ['an empty slug fails open', { testReceipt: 'missing', slug: '' }, [true]],
  ['no goalplan fails open', { testReceipt: 'missing', omitPlan: true }, [true]],
  ['no finalGate fails open', { testReceipt: 'missing', omitGate: true }, [true]],
  ['no session id fails open', { testReceipt: 'missing', session: '' }, [true]],
  ['a corrupt goalplan fails open', { testReceipt: 'missing', planRaw: '{not json' }, [true]],
  // the shapes the chain reads
  ['the marker counts anywhere in the packet', { testReceipt: 'missing', packet: 'x ' + guard.FINAL_GATE_MARKER + ' y' }, [false]],
  ['a lower-case marker is not the marker', { testReceipt: 'missing', packet: guard.FINAL_GATE_MARKER.toLowerCase() }, [true]],
  ['a slug that is not a string fails open', { testReceipt: 'missing', stateRaw: JSON.stringify({ sessionId: SESSION, slug: 5 }) }, [true]],
  ['a session state that is an array fails open', { testReceipt: 'missing', stateRaw: '[]' }, [true]],
  ['a goalplan that is an array fails open', { testReceipt: 'missing', planRaw: '[]' }, [true]],
  ['a goalplan with trailing data fails open', { testReceipt: 'missing', planRaw: '{"finalGate":{}} x' }, [true]],
  ['a goalplan with NaN fails open', { testReceipt: 'missing', planRaw: '{"n":NaN,"finalGate":{},"criteria":[]}' }, [true]],
  ['a goalplan number past the double range still parses', { testReceipt: 'missing', planRaw: '{"n":1e999,"finalGate":{},"criteria":[]}' }, [false, /test receipt path is not recorded/]],
  ['a null finalGate fails open', { testReceipt: 'missing', gate: null }, [true]],
  ['a finalGate that is a string fails open', { testReceipt: 'missing', gate: 'x' }, [true]],
  ['a finalGate that is an array passes the typeof test and records no path', { gate: [] }, [false, /test receipt path is not recorded/]],
  ['criteria that is not an array needs no QA receipt', { testReceipt: HERE, criteria: { surface: 'web' } }, [true]],
  ['criteria elements that are not objects are skipped and a visual one among them demands QA', { testReceipt: HERE, criteria: [null, 5, 'web', [], { surface: 'web' }] }, [false, /QA receipt path is not recorded/]],
  ['a surface that is not a string is not visual', { testReceipt: HERE, criteria: [{ surface: ['web'] }] }, [true]],
  ['finalGate.qaRequired does not decide, the criteria do', { testReceipt: HERE, gate: { qaRequired: true, testReceiptPath: EVID + 'test.json' } }, [true]],
  // receipt paths and what a receipt must hold
  ['a test receipt path that is not a string is not recorded', { testPath: 5 }, [false, /test receipt path is not recorded/]],
  ['an empty test receipt path is not recorded', { testPath: '' }, [false, /test receipt path is not recorded/]],
  ['a receipt that is a directory is unreadable', { testReceipt: 'dir' }, [false, NOREC]],
  ['a receipt that is not JSON is unreadable', { testReceipt: '{nope' }, [false, NOREC]],
  ['a receipt that is an array is unreadable', { testReceipt: '[]' }, [false, NOREC]],
  ['a receipt without sourceIdentity is unreadable', { testReceipt: '{"kind":"test"}' }, [false, NOREC]],
  ['an identity with an unknown kind is unreadable', { testReceipt: { ...HERE, kind: 'bogus' } }, [false, NOREC]],
  ['an identity whose commitSha is not a string is unreadable', { testReceipt: { ...HERE, commitSha: 5 } }, [false, NOREC]],
  ['an identity whose dirty is not a boolean is unreadable', { testReceipt: { ...HERE, dirty: 'no' } }, [false, NOREC]],
  ['an identity with a null sourceRoot is unreadable', { testReceipt: { ...HERE, sourceRoot: null } }, [false, NOREC]],
  ['an identity with a relative sourceRoot is unreadable', { testReceipt: { ...HERE, sourceRoot: 'rel/tree' } }, [false, NOREC]],
  ['a receipt path that is absolute is read as written', { testReceipt: HERE, testPath: WS + '/' + EVID + 'test.json' }, [true]],
  ['a receipt path with dot segments is cleaned', { testReceipt: HERE, testPath: EVID + '../evidence/test.json' }, [true]],
  ['a receipt that is a symlink is followed', { testReceipt: HERE, link: 'test.json', testPath: EVID + 'link.json' }, [true]],
  ['a receipt path outside the working directory is looked for there', { testPath: '../outside.json' }, [false, /missing, empty or unreadable: \.\.\/outside\.json/]],
  // identities and the refusal text
  ['an unavailable receipt identity is never stale', { testReceipt: NO_GIT }, [true]],
  ['a receipt with a source root against a tree without one is stale', { testReceipt: SRC }, [false, /aaaaaaa, but the tree is now aaaaaaa/]],
  ['the same source root on both sides is not stale', { testReceipt: SRC, capture: SRC }, [true]],
  ['different source roots are stale', { testReceipt: SRC, capture: { ...HERE, sourceRoot: '/src/other' } }, [false]],
  ['a dirty receipt and a dirty tree show +dirty on both sides', { testReceipt: { ...MOVED, dirty: true }, capture: { ...HERE, dirty: true, treeHash: 't' } }, [false, /bbbbbbb\+dirty, but the tree is now aaaaaaa\+dirty/]],
  ['a short commit sha is shown whole', { testReceipt: { ...HERE, commitSha: 'abc' } }, [false, /produced against abc,/]],
  ['an empty commit sha is shown empty', { testReceipt: { ...HERE, commitSha: '' } }, [false]],
  ['a long commit sha is cut at seven characters', { testReceipt: { ...HERE, commitSha: '0123456789abcdef0123456789abcdef01234567' } }, [false, /against 0123456,/]],
  ['astral characters in a sha count as two code units each', { testReceipt: { ...HERE, commitSha: '\u{1F600}\u{1F600}\u{1F600}abcdef' } }, [false, /against \u{1F600}\u{1F600}\u{1F600}a,/u]],
  ['a missing line comes before a stale line whatever the slot order', { surface: 'web', testReceipt: MOVED }, [false, /QA receipt path is not recorded[^]*test receipt was produced/]],
  ['a missing test receipt comes before a stale QA receipt', { surface: 'web', testReceipt: 'missing', qaReceipt: MOVED }, [false, /test receipt path is not recorded[^]*QA receipt was produced/]],
  ['stale lines keep the slot order', { surface: 'web', testReceipt: MOVED, qaReceipt: { ...MOVED, commitSha: 'ccccccc' } }, [false, /test receipt was produced[^]*QA receipt was produced/]],
  // source identity
  ['a corrupt source binding refuses with SOURCE-ROOT', { testReceipt: HERE, files: { '.codexclaw/sources/sess-1.json': '{not json\n' } }, [false, /SOURCE-ROOT/]],
  ['SOURCE-ROOT is decided before any receipt is read', { testReceipt: 'missing', files: { '.codexclaw/sources/sess-1.json': '{not json\n' } }, [false, /SOURCE-ROOT/]],
  ['a pinned worktree without its binding refuses with SOURCE-ROOT', { testReceipt: HERE, state: { phase: 'B', boundSourceRoot: '/src/tree' } }, [false, /SOURCE-ROOT/]],
  ['a capture that throws refuses with SOURCE-ROOT', { testReceipt: HERE, capture: 'throw' }, [false, /SOURCE-ROOT/]],
  ['a session id that is not canonical refuses with SOURCE-ROOT', { testReceipt: HERE, session: 'sess 1' }, [false, /SOURCE-ROOT/]],
  ['a session id that is not canonical and has no state fails open', { testReceipt: HERE, session: 'sess 2' }, [true]],
  ['without a capture function a directory without git has no identity and is never stale', { testReceipt: MOVED, capture: undefined }, [true]],
];

const results = [];
for (const [name, options, want] of cases) {
  const f = fixture(options);
  const cwd = fs.mkdtempSync(path.join(scratch, 'fg-'));
  const put = text => text.split(WS).join(cwd);
  const back = text => text.split(cwd).join(WS);
  for (const d of f.dirs) fs.mkdirSync(path.join(cwd, d), { recursive: true });
  for (const [rel, body] of Object.entries(f.files)) {
    fs.mkdirSync(path.dirname(path.join(cwd, rel)), { recursive: true });
    fs.writeFileSync(path.join(cwd, rel), put(body));
  }
  for (const [rel, target] of Object.entries(f.symlinks)) fs.symlinkSync(target, path.join(cwd, rel));
  const capture = f.capture === undefined ? undefined : f.capture === 'throw' ? () => { throw new Error('boom'); } : () => f.capture;
  const answer = guard.checkFinalGatePrereqs(put(f.packet), f.session, cwd, capture);
  assert.equal(answer.ok, want[0], name);
  if (want[1]) assert.match(answer.reason ?? '', want[1], name);
  const oracleAnswer = { ok: answer.ok, ...(answer.reason === undefined ? {} : { reason: back(answer.reason) }) };
  const expected = { ok: answer.ok, ...(answer.reason === undefined ? {} : { reason: rename(back(answer.reason)) }) };
  const same = JSON.stringify(oracleAnswer) === JSON.stringify(expected);
  results.push({
    name, packet: rename(f.packet), session: f.session,
    files: Object.fromEntries(Object.entries(f.files).map(([k, v]) => [rename(k), rename(v)])),
    dirs: f.dirs.map(rename), symlinks: Object.fromEntries(Object.entries(f.symlinks).map(([k, v]) => [rename(k), v])),
    capture: f.capture === undefined ? null : f.capture,
    oracle: oracleAnswer, expected, classification: same ? 'identical' : 'intentionally-changed',
    ...(same ? {} : { reason: 'CRW names: the refusal prefix "[crw — final gate]" (R23) and the evidence path ".crw/evidence/" (R26).' }),
  });
  fs.rmSync(cwd, { recursive: true, force: true });
}
fs.writeFileSync(output, JSON.stringify({
  oracle: 'CXC v0.2.40 (3c1459ac)', source: 'subagent-config/src/final-gate-guard.ts:1-189',
  tests: 'subagent-config/test/final-gate-guard.test.ts:88-229', cases: results,
}, null, 2) + '\n');
console.log(results.length + ' cases recorded; ' + results.filter(r => r.classification !== 'identical').length + ' differ by name substitution.');
