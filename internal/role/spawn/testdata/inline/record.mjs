import fs from 'node:fs';
import path from 'node:path';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import { stripTypeScriptTypes } from 'node:module';
const [oracle, scratch, output] = process.argv.slice(2);
const prefix = 'plugins/codexclaw/components/subagent-config/';
const source = fs.readFileSync(path.join(oracle, prefix, 'src/spawn-attach-hook.ts'), 'utf8');
const tests = fs.readFileSync(path.join(oracle, prefix, 'test/spawn-attach-hook.test.ts'), 'utf8');
const slice = source.slice(source.indexOf('export const LEAF_GUARD_MARKER'), source.indexOf('/** True when the hook stdin')) + source.slice(source.indexOf('export const INLINE_SKILL_OPEN'), source.indexOf('/**\n * Decide the hook output'));
const names = ['inlineSkillBodies', 'skillAffordanceBlock', 'buildLeafSkillCatalog', 'mentionedFolders', 'LEAF_GUARD_BLOCK', 'LEAF_GUARD_BLOCK_COORDINATOR', 'V1_SCOPE_BLOCK', 'V1_SCOPE_BLOCK_COORDINATOR'];
const code = stripTypeScriptTypes(slice.replaceAll('export ', ''));
const api = new Function('readdirSync', 'readFileSync', 'existsSync', 'resolve', 'skillPath', 'MAX_NORMALIZE_LENGTH', code + '\nreturn {' + names.join(',') + '};')(fs.readdirSync, fs.readFileSync, fs.existsSync, path.resolve, (dir, folder) => { const p = path.resolve(dir, folder, 'SKILL.md'); return fs.existsSync(p) ? p : null; }, 256 * 1024);
fs.mkdirSync(scratch, { recursive: true });
const skillsDir = path.join(scratch, 'skills');
const bodies = {};
for (const folder of ['dev', 'search', 'dev-testing', 'loop', 'pabcd']) {
  bodies[folder] = '---\nname: cxc-' + folder + '\ndescription: "Synthetic ' + folder + ' skill for direct oracle tests."\n---\n# ' + folder + '\nUse $cxc-search only if the task calls for it.\n';
  fs.mkdirSync(path.join(skillsDir, folder, 'references'), { recursive: true });
  fs.writeFileSync(path.join(skillsDir, folder, 'SKILL.md'), bodies[folder]);
  fs.writeFileSync(path.join(skillsDir, folder, 'references/development-practice.md'), 'Detail reference is not automatically loaded.\n');
}
const symbolic = s => s.replaceAll(skillsDir, '${SKILLS}');
const rename = s => s.replaceAll('$codexclaw:cxc-', '$crw:crw-').replaceAll('cxc-', 'crw-').replaceAll('[CXC-', '[CRW-').replaceAll('cxc orchestrate', 'crw orchestrate').replaceAll('cxc loop', 'crw loop').replaceAll('cxc orchestration', 'crw orchestration').replace(/\/(dev|search|dev-testing)(?=\/SKILL\.md)/g, '/crw-$1');
const groups = [];
let group;
const wrap = op => (...args) => {
  const actual = api[op](...args);
  const input = op === 'inlineSkillBodies' || op === 'mentionedFolders' ? args[0] : '';
  const value = actual instanceof Set ? [...actual].sort() : symbolic(actual);
  const row = { op, classification: 'intentionally-changed', reason: 'CRW skill/namespace/folder, inline tag and marker substitutions.' };
  if (Array.isArray(value)) { row.input = rename(symbolic(input)); row.folders = value.map(f => 'crw-' + f); }
  else if (input.length > 4096) {
    const flood = input.startsWith('<skill name="cxc-dev">');
    const n = flood ? input.split('<skill name="cxc-dev">').length - 1 : input.match(/^[xy]+/)[0].length;
    row.recipe = { unit: flood ? '<skill name="crw-dev">' : input[0], n, closes: flood ? input.split('</skill>').length - 1 : 0, tail: rename(flood ? ' $cxc-search' : input.slice(n)) };
    row.identity = actual === input;
    row.suffix = rename(symbolic(actual.slice(input.length)));
    const digest = crypto.createHash('sha256').update(actual).digest('hex');
    row.oracleDigestPieces = [digest.slice(0, 32), digest.slice(32)];
    row.oracleUnits = actual.length;
  } else { row.input = rename(symbolic(input)); row.oracle = value; row.expected = rename(value); }
  group.cases.push(row);
  return actual;
};
const direct = Object.fromEntries(['inlineSkillBodies', 'skillAffordanceBlock', 'buildLeafSkillCatalog', 'mentionedFolders'].map(op => [op, wrap(op)]));
for (const match of tests.matchAll(/test\("([^"\n]+)", \(\) => \{([\s\S]*?)\n\}\);/g)) {
  const [raw, name, body] = match;
  if (!/\b(?:inlineSkillBodies|skillAffordanceBlock|buildLeafSkillCatalog|mentionedFolders)\(/.test(body) || /runSpawnAttachHook\(|updatedInputOf\(/.test(body)) continue;
  group = { number: groups.length + 1, name, cases: [] };
  new Function('test', 'assert', 'SKILLS_DIR', 'inlineSkillBodies', 'skillAffordanceBlock', 'buildLeafSkillCatalog', 'mentionedFolders', 'INLINE_SKILL_OPEN', 'SKILL_AFFORDANCE_MARKER', 'readFileSync', 'join', 'process', stripTypeScriptTypes(raw))((_, callback) => callback(), assert, skillsDir, direct.inlineSkillBodies, direct.skillAffordanceBlock, direct.buildLeafSkillCatalog, direct.mentionedFolders, '<skill name="cxc-', '[CXC-SKILL-AFFORDANCE]', fs.readFileSync, path.join, process);
  groups.push(group);
}
assert.equal(groups.length, 13);
const guards = Object.fromEntries(names.filter(n => n.includes('BLOCK')).map(n => [n, { oracle: api[n], expected: rename(api[n]), classification: 'intentionally-changed', reason: 'CRW markers and command wording.' }]));
const linked = path.join(scratch, 'linked');
const outside = path.join(scratch, 'outside');
fs.mkdirSync(path.join(linked, 'dev'), { recursive: true });
fs.mkdirSync(outside, { recursive: true });
fs.writeFileSync(path.join(outside, 'SKILL.md'), 'name: cxc-dev\ndescription: "outside sentinel"\nOUTSIDE-CONTENT\n');
fs.symlinkSync(path.join(outside, 'SKILL.md'), path.join(linked, 'dev', 'SKILL.md'));
fs.symlinkSync(outside, path.join(linked, 'search'));
const security = { input: 'use $crw-dev and $crw-search', oracleInline: api.inlineSkillBodies('use $cxc-dev and $cxc-search', linked).replaceAll(linked, '${LINKED}'), oracleCatalog: api.buildLeafSkillCatalog(linked).replaceAll(linked, '${LINKED}'), expectedInline: 'use $crw-dev and $crw-search', expectedCatalog: '', classification: 'intentionally-changed', reason: 'Security: reject linked SKILL.md and linked skill folder, including reads outside the caller root.' };
assert.ok(security.oracleInline.includes('OUTSIDE-CONTENT'));
assert.ok(security.oracleCatalog.includes('outside sentinel'));
const decoder = [[0xe2,0x82],[0xed,0xa0,0x80],[0xf0,0x9f,0x92],[0xc0,0xaf],[0xe2,0x28,0xa1],[0xef,0xbb,0xbf],[0xf4,0x90,0x80,0x80],[0x80,0x80]].map(bytes => ({ bytes, expected: Buffer.from(bytes).toString('utf8') }));
const metadata = ['name:\u00a0cxc-dev\r\ndescription:\t"line"\r\n', '# body\nname: cxc-other\ndescription: "body metadata"\n', 'name:\nnext-line-name\ndescription:\nnext-line-description\n', 'name: cxc-dev\ndescription: has "quotes" in middle\n', 'name: cxc-dev\ndescription: "single"\u2028name: extra\n', 'name: cxc-dev\ndescription: ' + 'x'.repeat(130) + '\n'];
const catalogCases = metadata.map((body, i) => { const dir = path.join(scratch, 'metadata-' + i); fs.mkdirSync(path.join(dir, 'dev'), {recursive:true}); fs.writeFileSync(path.join(dir, 'dev/SKILL.md'), body); const result=api.buildLeafSkillCatalog(dir).replaceAll(dir, '${SKILLS}'); return { body: rename(body), oracle: result, expected: rename(result), classification: 'intentionally-changed', reason: 'CRW metadata name substitution; regex quirks retained.' }; });
fs.writeFileSync(output, JSON.stringify({ oracle: 'CXC v0.2.40 (3c1459ac)', source: 'subagent-config/src/spawn-attach-hook.ts:284-350,596-848', tests: 'subagent-config/test/spawn-attach-hook.test.ts:804-810,835-841,879-963,1172-1200', skills: Object.fromEntries(Object.entries(bodies).map(([folder, body]) => ['crw-' + folder, rename(body)])), groups, guards, security, decoder, catalogCases }, null, 2) + '\n');
console.log('Recorded ' + groups.length + ' original B-class callbacks; guard strings and unsafe linked reads characterized.');
