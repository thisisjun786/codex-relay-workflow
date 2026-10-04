// CXC v0.2.40 (3c1459ac), pabcd-state/src/scan-cli.ts:1-210.
// Parser tests: test/scan-cli.test.ts:27-70,179-198,340-357, plus edge cases.
// Usage: node record-oracle.mjs <oracle dist file URL> <output directory>
// Only the output directory is written; the oracle is imported read-only.
import {mkdirSync, writeFileSync} from 'node:fs';
import {resolve} from 'node:path';
const [dist, out] = process.argv.slice(2);
const {parseScanCliArgs} = await import(dist + '/scan-cli.js');
const cases = [];
const add = (id, argv, cwd = '/ws') => cases.push({id, argv, cwd});
const record = (id, ...argv) => add(id, ['record', '--session', 's', ...argv]);
for (const [id, argv] of [
  ['no_action', []], ['missing_session', ['record']], ['unknown_action', ['evidence']],
  ['show', ['show']], ['action_case', ['Record']], ['empty_action', ['']],
  ['help', ['help']], ['dash_help', ['--help']], ['short_help', ['-h']],
  ['help_ignores_rest', ['help', '--cwd', '/other', '--nope']],
  ['help_case', ['HELP']], ['session_consumes_flag', ['record', '--session', '--derive']],
  ['missing_session_beats_count', ['record', '--contradictions', '-1']],
  ['pair_beats_session', ['record', '--dim', 'nope=high']],
]) add(id, argv);
add('default_shape', ['record', '--session', 's1'], '/some/cwd');
for (const flag of ['--session', '--cwd', '--contradictions', '--high', '--map', '--dim', '--known', '--unknown', '--confidence']) {
  record('missing_' + flag.slice(2), flag);
  record('flag_value_' + flag.slice(2), flag, '--derive');
}
record('unknown_flag', '--nope'); record('positional', 'text'); record('record_help', '--help');
record('equals_flag', '--session=x'); record('double_dash', '--');
record('empty_session', '--session', ''); record('whitespace_session', '--session', ' ');
record('repeated_session', '--session', 'last'); record('empty_cwd', '--cwd', '');
record('repeated_cwd', '--cwd', '/one', '--cwd', '/two');
record('cwd_missing_resets', '--cwd', '/one', '--cwd');
record('unknown_beats_counts', '--contradictions', 'abc', '--nope');
record('count_error_order', '--contradictions', 'abc', '--high', '-1');
for (const flag of ['--contradictions', '--high']) {
  const values = ['-1', 'abc', '', ' ', '0', '-0', '-0.1', '+5', ' 7 ', '\uFEFF3', '\u00853', '５',
    '12abc', '1.9', '1e3', '0x10', 'Infinity', '1_000', '900719925474099267', '9'.repeat(20),
    '1' + '0'.repeat(21), '9'.repeat(309), '0'.repeat(400) + '1'];
  values.forEach((v, i) => record(flag.slice(2) + '_' + i, flag, v));
  record('repeated_' + flag.slice(2), flag, 'bad', flag, '2');
}
for (const flag of ['--map', '--dim', '--known', '--unknown', '--confidence']) {
  for (const [i, pair] of ['goal', '=high', '', 'goal=', 'nope=high', 'GOAL=high'].entries())
    record(flag.slice(2) + '_pair_' + i, flag, pair);
}
for (const dim of ['goal', 'constraint', 'success', 'ontology']) {
  for (const level of ['low', 'mid', 'high', 'max', 'enormous', 'HIGH']) record(dim + '_' + level, '--dim', dim + '=' + level);
  record('map_' + dim, '--map', 'q=' + dim);
}
for (const key of ['__proto__', 'toString', 'constructor', 'hasOwnProperty', 'q']) record('map_key_' + key, '--map', key + '=goal');
record('map_extra_equals', '--map', 'q=goal=success'); record('map_empty_key', '--map', '=goal');
record('map_repeated', '--map', 'q=goal', '--map', 'q=success');
record('dim_repeated', '--dim', 'goal=high', '--dim', 'goal=low');
for (const flag of ['--known', '--unknown']) {
  record(flag.slice(2) + '_equals_text', flag, 'goal=uses http://x?a=b:c');
  record(flag.slice(2) + '_space', flag, 'goal=   ');
  record(flag.slice(2) + '_duplicates', flag, 'goal=fact', flag, 'goal=fact', flag, 'success=other');
}
const confidence = ['0.5abc', 'abc', '', ' ', '1.5', '-0.1', '0.5', '0', '1', '-0', '-1e-999', '1e-999',
  '.5', '1.', '+.5', '1e-1', '0x1', '0X1', '0o1', '0b1', '0x' + '0'.repeat(100) + '1', '0x10000000000000000',
  '0x', '+0x1', '-0x0', '0x1p0', '0b2', '1_0', 'Inf', 'Infinity', '-Infinity', 'NaN', '1e999', ' .5 ', '\uFEFF.5', '\u0085.5', '1e', '.', '+', '0x0'];
confidence.forEach((v, i) => record('confidence_' + i, '--confidence', 'goal=' + v));
record('confidence_repeated', '--confidence', 'goal=0.2', '--confidence', 'goal=0.7');
record('user_text_keeps_names', '--known', 'goal=cxc scan', '--unknown', 'success=cxc orchestrate');
record('all_fields', '--derive', '--derive', '--map', 'q=constraint', '--dim', 'goal=high', '--known', 'goal=a=b',
  '--unknown', 'success=gap', '--confidence', 'ontology=0.5', '--contradictions', '2', '--high', '1', '--cwd', '/other');
for (const c of cases) {
  const result = parseScanCliArgs(c.argv, c.cwd);
  c.negativeZero = {};
  for (const key of ['contradictionCount', 'highContradictionCount'])
    if (Object.is(result[key], -0)) c.negativeZero[key] = true;
  for (const [key, value] of Object.entries(result.confidence ?? {}))
    if (Object.is(value, -0)) c.negativeZero['confidence.' + key] = true;
  c.want = result;
}
mkdirSync(out, {recursive: true});
writeFileSync(resolve(out, 'oracle.json'), JSON.stringify({oracle: 'CXC v0.2.40 3c1459ac', cases}, null, 2) + '\n');
console.log(`recorded ${cases.length} parser answers`);
