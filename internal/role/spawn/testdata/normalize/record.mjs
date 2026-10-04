import fs from 'node:fs';
import path from 'node:path';
import assert from 'node:assert/strict';
import { stripTypeScriptTypes } from 'node:module';
import { performance } from 'node:perf_hooks';
const [oracle, scratch, output] = process.argv.slice(2);
const sourcePath = path.join(oracle, 'plugins/codexclaw/components/subagent-config/src/spawn-attach-hook.ts');
const testPath = path.join(oracle, 'plugins/codexclaw/components/subagent-config/test/spawn-attach-hook.test.ts');
const source = fs.readFileSync(sourcePath, 'utf8');
const slice = source.slice(source.indexOf('const MAX_NORMALIZE_LENGTH ='), source.indexOf('// ── 260709 leaf-agent'));
const normalize = new Function('existsSync', 'resolve', 'basename', stripTypeScriptTypes(slice.replace('export function normalizeSkillMentions', 'function normalizeSkillMentions')) + '\nreturn normalizeSkillMentions;')(fs.existsSync, path.resolve, path.basename);
const skills = path.join(scratch, 'plugin/skills');
for (const folder of ['dev', 'search']) {
  fs.mkdirSync(path.join(skills, folder), { recursive: true });
  fs.writeFileSync(path.join(skills, folder, 'SKILL.md'), '# synthetic skill\n');
}
fs.mkdirSync(path.join(skills, '../.codex-plugin'), { recursive: true });
fs.writeFileSync(path.join(skills, '../.codex-plugin/plugin.json'), '{"name":"codexclaw"}');
const roots = [[skills, '${SKILLS}']];
let alternate = 0, unsafe = 0;
const temp = prefix => {
  const dir = fs.mkdtempSync(prefix);
  const key = prefix.includes('cxc skills ') ? '${UNSAFE' + ++unsafe + '}' : '${ALT' + ++alternate + '}';
  roots.push([dir, key]);
  return dir;
};
fs.mkdirSync(path.join(scratch, 'tmp'), { recursive: true });
const symbolic = text => roots.slice().sort((a, b) => b[0].length - a[0].length).reduce((s, [dir, key]) => s.split(dir).join(key), text);
const rename = text => text.replaceAll('$codexclaw:cxc-', '$crw:crw-').replaceAll('$cxc-', '$crw-').replace(/(\$\{(?:SKILLS|ALT\d+|UNSAFE\d+)\})\/(dev|search)(?=\/)/g, '$1/crw-$2');
const groups = [];
let group;
const record = (message, dir) => {
  const result = normalize(message, dir);
  if (message.length < 32768) {
    group.cases.push({ input: rename(symbolic(message)), skills: symbolic(dir), oracle: symbolic(result), expected: rename(symbolic(result)), classification: result === rename(result) && message === rename(message) ? 'identical' : 'intentionally-changed', reason: 'CRW skill prefix and full crw-X folder substitution (R1/R3/R10 and folder=name).' });
  }
  return result;
};
const test = (name, fn) => {
  group = { number: groups.length + 1, name: name.replace('mention normalization: ', ''), cases: [] };
  if (group.number === 21) group.recipe = 'adversarial-floods';
  if (group.number === 22) group.recipe = 'over-limit';
  groups.push(group);
  fn();
};
const tests = fs.readFileSync(testPath, 'utf8');
const selected = tests.slice(tests.indexOf('test("mention normalization:'), tests.indexOf('test("leaf guard text'));
const compiled = stripTypeScriptTypes(selected);
new Function('test', 'assert', 'normalizeSkillMentions', 'SKILLS_DIR', 'canonicalSkillMention', 'mkdtempSync', 'join', 'tmpdir', 'mkdirSync', 'dirname', 'writeFileSync', 'rmSync', 'readFileSync', 'resolve', 'performance', 'process', 'console', compiled)(test, assert, record, skills, folder => `[$cxc-${folder}](skill://${path.join(skills, folder, 'SKILL.md')})`, temp, path.join, () => path.join(scratch, 'tmp'), fs.mkdirSync, path.dirname, fs.writeFileSync, fs.rmSync, fs.readFileSync, path.resolve, performance, process, { log() {} });
assert.equal(groups.length, 24);
fs.writeFileSync(output, JSON.stringify({ oracle: 'CXC v0.2.40 (3c1459ac)', source: 'subagent-config/src/spawn-attach-hook.ts:74-263', tests: 'subagent-config/test/spawn-attach-hook.test.ts:104-381', groups }, null, 2) + '\n');
console.log(`24 upstream test callbacks passed; ${groups.reduce((n, g) => n + g.cases.length, 0)} small calls recorded; large floods encoded as recipes.`);
