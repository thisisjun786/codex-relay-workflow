// Node 24 development recorder; Go tests replay its JSON without running Node.
// Usage: node record-agentthread-toml.mjs <oracle-root> <scratch-dir> <output.json>
import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const [root, scratch, output] = process.argv.slice(2);
if (!root || !scratch || !output || process.versions.node.split('.')[0] !== '24') throw Error('Node 24 and three paths required');
const sourcePath = 'plugins/codexclaw/components/pabcd-state/src/agent-thread-permissions.ts';
const source = readFileSync(join(root, sourcePath), 'utf8');
const fingerprint = ['025423bc', 'f27b7787', '987b87fe', 'ec0d803a', '0eaf2f71', '304347d2', '75b2ec66', 'eff8d1e6'];
if (createHash('sha256').update(source).digest('hex') !== fingerprint.join('')) throw Error('oracle source changed');
if (resolve(scratch).startsWith(resolve(root) + '/') || resolve(scratch) === resolve(root)) throw Error('scratch must be outside oracle');
mkdirSync(scratch, { recursive: true });
const copy = join(scratch, 'agent-thread-permissions.ts');
const exports = '\nexport {finiteNumber, validDateTime, table, keyPath, assignKey, enterTable, validValue};\n';
writeFileSync(copy, source + exports);
if (readFileSync(copy, 'utf8') !== source + exports) throw Error('scratch copy differs');
const m = await import(pathToFileURL(copy));
const cases = [];
function boolCases(unit, inputs) {
  for (const input of inputs) {
    const row = { unit, input };
    try { row.answer = m[unit](input); } catch (e) { row.error = e.name; }
    cases.push(row);
  }
}
const numbers = [
  '0', '+0', '-0', '1_000', '9007199254740993', '9223372036854775807', '9223372036854775808',
  '-9223372036854775808', '-9223372036854775809', '+9223372036854775808', '9'.repeat(100),
  '0x7fff_ffff_ffff_ffff', '0x8000000000000000', '0Xf', '0o777', '0b101', '0xA_B',
  '0o7_1', '0b1_0', '+0xf', '-0xf', '+0xe', '-0xe', '012', '123x', '0x', '0b2', '0x-1', '0x+1', '0o-1', '0b+1',
  '1e300', '1e309', '1e9999', '-1e9999', '1e-400', '0.000', '1_000.2_50e+1_0',
  '1.', '.1', '1e', '1e+', '1_','1__2','1._2','1.2__3','1e_2','1e2_','0x_1','0x1__2','0o7_','0b1__0',
  'inf', '+inf', '-inf', 'nan', '-nan', 'NaN', 'true', 'false', '', '1 ', '1\uFEFF', '1e2 ',
];
const dates = [
  '2000-02-29', '1900-02-29', '2024-02-29', '2023-02-29', '0000-02-29', '0004-02-29',
  '0099-02-28', '0100-02-29', '2026-02-30', '2026-00-01', '2026-13-01', '2026-01-00',
  '2026-04-31', '9999-12-31', '2026-01-01T23:59:60Z', '2026-01-01t00:00:00z',
  '2026-01-01T24:00:00Z', '2026-01-01T00:60:00Z', '2026-01-01T00:00:61Z',
  '2026-01-01T00:00:00+23:59', '2026-01-01T00:00:00+24:00', '2026-01-01T00:00:00-00:60',
  '12:00:60', '24:00:00', '00:60:00', '00:00:61', '00:00:00.001', '1979-05-27 07:32:00Z',
  '12:61:00\n', '2000-02-30tail', 'not-a-date', '2026-1-1',
];
boolCases('finiteNumber', [...numbers, ...dates]);
boolCases('validDateTime', [...dates, 'true', '1e9999']);
const strings = [
  '""', "''", '"hello"', "'hello'", '"""hello\nworld"""', "'''one\ntwo'''", '"unterminated',
  '"a\nb"', "'a\rb'", '"""a\rb"""', "'''a\rb'''", "'\f'", '"""\f"""', "'''\f'''",
  '"\\b\\t\\n\\f\\r\\\"\\\\"', '"\\q"', '"""\\q"""', "'\\q'",
  '"\\u0000\\u00e9\\U0001F600"', '"\\uD800"', '"\\uDFFF"', '"\\U00110000"', '"\\uZZZZ"',
  '"\\u123"', '"\\U0000000"', '"\u0000"', '"\t"', '"\f"', '"\u000b"', '"\u007f"',
  '"""one\\\n  two"""', '"""one\\ \t\r\ntwo"""', '"""one\\ \rtwo"""',
  '"""one\\\n  \n two"""', '"""x""""', "'''x''''", '"""x"""""', "'''x'''''", '"a\u2028b"', '"😀"',
];
const compounds = [
  '[', '[1 2]', '[1,2,]', '[1,# comment\n2]', '[# comment\n]', '[true, [1], {a = 2}]',
  '{', '{a = 1 b = 2}', '{a=1,}', '{}', '{\n}', '{a=1,\nb=2}', '{a=1,a=2}', '{a=1,"a"=2}',
  '{a.b=1,a.c=2}', '{a=1,a.b=2}', '{a.b=1,a=2}', '{a = """one\ntwo"""}', "{a = '''one\ntwo'''}",
  '{a="\\\r"}', '{a="\\\u2028"}', '{a="\\\u2029"}', '{a=1# comment\n}', 'true false', 'true# comment',
  '["/tmp",\n"/tmp/roots",]', '{enabled=true,retries=2}',
];
for (const depth of [63, 64, 65, 66]) compounds.push('['.repeat(depth) + '0' + ']'.repeat(depth), '['.repeat(depth) + ']'.repeat(depth));
for (const space of ['\t','\n','\v','\f','\r',' ','\u00a0','\u1680','\u2000','\u200a','\u2028','\u2029','\u202f','\u205f','\u3000','\uFEFF','\u0085','\u200b']) compounds.push(`[${space}1${space}]`);
boolCases('validValue', [...numbers, ...dates, ...strings, ...compounds]);
const keys = [
  '', ' ', 'bare', '123_- = 1', ' alpha . \'beta gamma\' = 2', '"" = 1', "'' = 1", '"foo"', "'raw key'",
  'a.b.c', 'a.', 'a. ', 'a..b', 'a.\nb', 'bad key', 'x. bad key', '"unterminated', '"\\q"', '"\n"',
  '"\\b\\t\\n\\f\\r\\\"\\\\"', '"\\u0061"', '"\\U0001F600"', '"😀" = 1', '"\\uD800"',
  '"\\uDFFF"', '"\\U00110000"', '"\\uZZZZ"', '"\\u123"', '"\u007f"', 'é', 'aé', "'\\q'", '"\t"',
];
for (const input of keys) {
  const answer = m.keyPath(input);
  if (answer) answer.end = Buffer.byteLength(input.slice(0, answer.end));
  cases.push({ unit: 'keyPath', input, start: 0, answer });
}
for (const input of ['prefix: alpha . "😀" = 1', '😀: alpha = 1']) {
  const start = input.indexOf('alpha'), answer = m.keyPath(input, start);
  answer.end = Buffer.byteLength(input.slice(0, answer.end));
  cases.push({ unit: 'keyPath', input, start: Buffer.byteLength(input.slice(0, start)), answer });
}
function snapshot(entry) {
  const out = { kind: entry.kind };
  if (entry.children) out.children = Object.fromEntries([...entry.children].map(([k,v]) => [k,snapshot(v)]));
  if (entry.declared !== undefined) out.declared = entry.declared;
  if (entry.latest) out.latest = snapshot(entry.latest);
  return out;
}
const assign = (parts, scope) => ({ op: 'assign', parts, ...(scope ? {scope} : {}) });
const enter = (parts, array = false, save) => ({ op: 'enter', parts, array, ...(save ? {save} : {}) });
const sequences = [
  [], [assign([]), enter([])], [assign(['a']), assign(['a']), assign(['a','b'])],
  [assign(['a','b']), assign(['a','c']), assign(['a']), enter(['a']), enter(['a'])],
  [enter(['x']), assign(['a']), assign(['a']), enter(['x'])],
  [enter(['x'],true,'first'), assign(['key']), enter(['x'],true), assign(['key']), assign(['old'],'first')],
  [enter(['x'],true), enter(['x','nested']), assign(['a']), enter(['x'],true), enter(['x','nested'])],
  [enter(['x']), enter(['x'],true)], [enter(['x'],true), enter(['x'])],
  [assign(['x']), enter(['x']), enter(['x'],true)], [enter(['a','b']), enter(['a']), enter(['a','b'])],
  [assign(['a']), enter(['partial','a']), assign(['a']), assign(['a','b'])],
  [assign(['']), assign(['']), enter(['__proto__']), assign(['constructor'])],
];
for (const ops of sequences) {
  const root = m.table(), scopes = { root }; let current = root;
  const answers = [{ ok: true, root: snapshot(root), current: snapshot(current) }];
  for (const op of ops) {
    let ok;
    if (op.op === 'assign') ok = m.assignKey(op.scope ? scopes[op.scope] : current, op.parts);
    else { const next = m.enterTable(root, op.parts, op.array); ok = next !== null; if (next) current = next; if (op.save && next) scopes[op.save] = next; }
    answers.push({ ok, root: snapshot(root), current: snapshot(current) });
  }
  cases.push({ unit: 'structure', ops, answer: answers });
}
writeFileSync(output, JSON.stringify({ oracle: 'CXC v0.2.40 (3c1459ac)', source: sourcePath, fingerprint, node: process.version, cases }, null, 2) + '\n');
console.log(`Recorded ${cases.length} direct scanner cases`);
