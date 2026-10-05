// CXC v0.2.40 (3c1459ac), pabcd-state/src/review-round-cli.ts:1-209 and 308-317.
// Usage: node record-oracle.mjs <oracle dist directory> <output directory>
// Only the output directory is written; the oracle is imported read-only. Functions the oracle does not export (collectPlanFiles,
// v2SpawnSurface, renderOpenPacket) are reached through a copy of the compiled module in the output directory whose relative
// imports point back at the oracle and which exports them.
import {chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync} from 'node:fs';
import {dirname, resolve} from 'node:path';
import {pathToFileURL} from 'node:url';
const [dist, out] = process.argv.slice(2);
mkdirSync(out, {recursive: true});
const source = readFileSync(resolve(dist, 'review-round-cli.js'), 'utf8')
  .replace(/from "\.\/([a-z-]+)\.js"/g, (_, name) => 'from "' + pathToFileURL(resolve(dist, name + '.js')).href + '"')
  + '\nexport {collectPlanFiles, v2SpawnSurface, renderOpenPacket};\n';
const probe = resolve(out, 'instrumented.mjs');
writeFileSync(probe, source);
const O = await import(pathToFileURL(probe).href);
const home = mkdtempSync(resolve(out, 'home-'));
process.env.HOME = home;
const cases = {parse: [], hash: [], collect: [], recomputed: [], toml: [], packet: []};
// --- parser -------------------------------------------------------------------------------------------------
const parse = (id, argv, cwd = '/ws') => cases.parse.push({id, argv, cwd, want: O.parseReviewRoundCliArgs(argv, cwd)});
const O3 = ['open', '--session', 's'];
for (const [id, argv, cwd] of [
  ['no_args', []], ['help', ['help']], ['dash_help', ['--help']], ['short_help', ['-h']], ['help_upper', ['HELP']], ['short_help_upper', ['-H']],
  ['dash_help_upper', ['--HELP']], ['help_ignores_rest', ['help', '--cwd', '/other', '--nope']], ['open_help_is_not_help', ['open', '--help']],
  ['empty_verb', ['']], ['unknown_verb', ['close']], ['unknown_verb_case', ['Close', '--session', 's']], ['close_all', ['CLOSE']],
  ['dotted_i_verb', ['\u0130']], ['kelvin_verb', ['\u212A']], ['fullwidth_open', ['\uFF2F\uFF30\uFF25\uFF2E']], ['verb_with_space', [' open']],
  ['open', ['open']], ['show', ['show']], ['abort', ['abort']], ['open_upper', ['OPEN']], ['show_mixed', ['ShOw']], ['abort_upper', ['ABORT']],
  ['verb_after_flag', ['--session', 's', 'open']], ['default_cwd', ['open', '--session', 's1'], '/some/cwd'], ['relative_default_cwd', ['show'], 'rel/dir'],
  ['empty_default_cwd', ['show'], ''], ['plan_paths_accumulate', ['open', '--plan-path', 'a.md', '--plan-path', '', '--plan-path', 'b.md', '--plan-path', 'a.md']],
  ['cwd_missing_resets', ['open', '--cwd', '/one', '--cwd']], ['cwd_empty_kept', ['open', '--cwd', '']], ['cwd_last_wins', ['open', '--cwd', '/one', '--cwd', '/two']],
  ['session_consumes_flag', ['open', '--session', '--cwd', '/x']], ['reason_text_keeps_names', ['abort', '--reason', 'cxc review-round abort']],
  ['json_flag', ['show', '--json']], ['json_repeated', ['show', '--json', '--json']], ['json_value_ignored', ['show', '--json=false']],
  ['equals_forms_ignored', ['open', '--session=x', '--cwd=/y', '--plan-path=z.md', '--reason=r']], ['unknown_flag_ignored', ['open', '--nope', 'x', '--session', 's']],
  ['positional_ignored', ['open', 'stray', '--session', 's']], ['double_dash_ignored', ['open', '--', '--session', 's']],
  ['flag_case_sensitive', ['open', '--SESSION', 's', '--Json']], ['all_fields', ['abort', '--session', 's', '--cwd', '/w', '--plan-path', 'p.md', '--reason', 'why', '--json']],
  ['proto_like_values', ['open', '--session', '__proto__', '--reason', 'constructor', '--plan-path', 'toString']],
  ['unicode_values', ['open', '--session', '\u{1F600}', '--plan-path', '\u00e9.md']],
]) parse(id, argv, cwd);
for (const flag of ['--session', '--cwd', '--plan-path', '--reason']) {
  const name = flag.slice(2);
  parse('missing_' + name, ['open', flag]); parse('flag_value_' + name, ['open', flag, '--json']); parse('empty_' + name, ['open', flag, '']);
  parse('repeated_' + name, ['open', flag, 'one', flag, 'two']); parse('repeated_missing_' + name, ['open', flag, 'one', flag]);
}
cases.help = O.renderReviewRoundHelp();
// --- aggregate hash -----------------------------------------------------------------------------------------
const h = (c) => c.repeat(64);
const f = (path, sha256) => ({path, sha256});
for (const [id, files] of [
  ['empty', []], ['one', [f('devlog/_plan/u/000_plan.md', h('a'))]], ['two_in_order', [f('a.md', h('1')), f('b.md', h('2'))]],
  ['two_reversed_is_not_sorted', [f('b.md', h('2')), f('a.md', h('1'))]], ['same_path_twice', [f('a.md', h('1')), f('a.md', h('1'))]],
  ['nul_free_boundary', [f('a', 'b\u0000c'), f('d', 'e')]], ['non_ascii_path', [f('d\u00e9j\u00e0/000_\u65e5\u672c.md', h('f'))]],
  ['astral_path', [f('000_\u{1F600}.md', h('0'))]], ['lone_surrogate_path', [f('000_\ud800.md', h('0'))]], ['missing_sentinel', [f('a.md', 'missing')]],
]) cases.hash.push({id, files, want: O.planFilesHash(files)});
// --- the world the file readers run in ----------------------------------------------------------------------
const world = {
  dirs: ['ws/devlog/_plan/u/000_dir.md', 'ws/devlog/_plan/u/000_sub', 'ws/devlog/_plan/other', 'out'],
  files: {
    'ws/package.json': '{}\n', 'ws/devlog/_plan/u/000_plan.md': '# plan\n', 'ws/devlog/_plan/u/010_second.md': '# second\r\nwith crlf\r\n',
    'ws/devlog/_plan/u/notes.md': 'notes\n', 'ws/devlog/_plan/u/000_sub/x.md': 'in a subdirectory\n', 'ws/devlog/_plan/u/000_a\nb.md': 'newline in the name\n',
    'ws/devlog/_plan/u/000_a\rb.md': 'carriage return in the name\n', 'ws/devlog/_plan/u/\u0660\u0660\u0660_x.md': 'arabic-indic digits\n',
    'ws/devlog/_plan/u/000_.md': 'empty stem\n', 'ws/devlog/_plan/u/000_locked.md': 'unreadable\n', 'ws/devlog/_plan/other/000_other.md': '# other\n',
    'out/secret.md': 'TOP SECRET\n', 'out/000_x.md': 'outside doc\n',
  },
  symlinks: {
    'ws/devlog/_plan/u/020_link.md': '../../../../out/secret.md', 'ws/devlog/_plan/u/030_inlink.md': '000_plan.md', 'ws/devlog/_plan/u/040_dangle.md': 'nowhere.md',
    'ws/devlog/_plan/esc': '${ROOT}/out', 'ws/devlog/_plan/lnk': 'u', 'ws/devlog/_plan/u/000_subdirlink': '../../../../out',
    'ws/outlink.md': '../out/secret.md', 'ws/inlink.md': 'package.json', 'wslink': 'ws',
  },
  modes: {'ws/devlog/_plan/u/000_locked.md': 0},
};
const root = mkdtempSync(resolve(out, 'world-'));
const expand = (v) => typeof v === 'string' ? v.split('${ROOT}').join(root) : v;
const shorten = (v) => typeof v === 'string' ? v.split(root).join('${ROOT}') : v;
for (const d of world.dirs) mkdirSync(resolve(root, d), {recursive: true});
for (const [p, text] of Object.entries(world.files)) { mkdirSync(dirname(resolve(root, p)), {recursive: true}); writeFileSync(resolve(root, p), text); }
for (const [p, target] of Object.entries(world.symlinks)) { mkdirSync(dirname(resolve(root, p)), {recursive: true}); symlinkSync(expand(target), resolve(root, p)); }
for (const [p, mode] of Object.entries(world.modes)) chmodSync(resolve(root, p), mode);
const R = '${ROOT}', U = 'devlog/_plan/u';
for (const [id, unit, paths, cwd = R + '/ws'] of [
  ['no_paths', U, []], ['one_ok', U, [U + '/000_plan.md']], ['two_reversed_sorted', U, [U + '/010_second.md', U + '/000_plan.md']],
  ['same_file_three_spellings', U, [U + '/000_plan.md', './' + U + '/../u/000_plan.md', R + '/ws/' + U + '/000_plan.md']],
  ['absolute_unit', R + '/ws/' + U, [U + '/000_plan.md']], ['relative_unit_trailing_slash', U + '/', [U + '/000_plan.md']],
  ['package_json_outside_unit', U, ['package.json']], ['sibling_unit', U, ['devlog/_plan/other/000_other.md']], ['parent_climb', U, ['../out/secret.md']],
  ['absolute_outside', U, [R + '/out/secret.md']], ['first_ok_then_outside', U, [U + '/000_plan.md', 'package.json']], ['not_numbered', U, [U + '/notes.md']],
  ['empty_stem', U, [U + '/000_.md']], ['subdirectory_document', U, [U + '/000_sub/x.md']], ['newline_in_name', U, [U + '/000_a\nb.md']],
  ['carriage_return_in_name', U, [U + '/000_a\rb.md']], ['arabic_indic_digits', U, [U + '/\u0660\u0660\u0660_x.md']],
  ['directory_named_like_a_doc', U, [U + '/000_dir.md']], ['missing_file', U, [U + '/050_missing.md']], ['file_symlink_to_outside', U, [U + '/020_link.md']],
  ['file_symlink_to_inside', U, [U + '/030_inlink.md']], ['dangling_symlink', U, [U + '/040_dangle.md']], ['unit_itself', U, [U]], ['empty_string_path', U, ['']],
  ['unit_symlinked_outside', 'devlog/_plan/esc', ['devlog/_plan/esc/000_x.md']], ['unit_symlinked_inside', 'devlog/_plan/lnk', ['devlog/_plan/lnk/000_plan.md']],
  ['subdirectory_symlinked_outside', U, [U + '/000_subdirlink/000_x.md']], ['cwd_is_a_symlink', U, [U + '/000_plan.md', U + '/010_second.md'], R + '/wslink'],
  ['cwd_link_physical_spelling', R + '/ws/' + U, [R + '/ws/' + U + '/000_plan.md'], R + '/wslink'],
  ['unit_outside_cwd', R + '/out', [R + '/out/000_x.md']], ['unreadable_file', U, [U + '/000_locked.md']],
]) {
  let want;
  try { want = O.collectPlanFiles(expand(cwd), expand(unit), paths.map(expand)); } catch (e) { want = {throws: e.code ?? String(e)}; }
  cases.collect.push({id, cwd, unit, paths, want: JSON.parse(shorten(JSON.stringify(want)))});
}
for (const [id, paths, cwd = R + '/ws'] of [
  ['empty', []], ['present_and_missing', [U + '/000_plan.md', U + '/050_missing.md']],
  ['order_and_duplicates_kept', [U + '/010_second.md', U + '/000_plan.md', U + '/010_second.md']], ['dot_dot_back_inside', ['devlog/../package.json']],
  ['directory', [U + '/000_dir.md', U]], ['empty_path_is_cwd', ['']], ['absolute_inside', [R + '/ws/package.json']], ['absolute_outside', [R + '/out/secret.md']],
  ['parent_climb_outside', ['../out/secret.md']], ['symlink_to_inside', ['inlink.md']], ['symlink_to_outside', ['outlink.md']],
  ['symlinked_unit_inside', ['devlog/_plan/lnk/000_plan.md']], ['symlinked_unit_outside', ['devlog/_plan/esc/000_x.md']],
  ['symlinked_subdirectory_outside', [U + '/000_subdirlink/secret.md']], ['dangling_symlink', [U + '/040_dangle.md']],
  ['cwd_is_a_symlink', ['package.json', 'inlink.md', 'outlink.md'], R + '/wslink'], ['crlf_content_hashed_raw', [U + '/010_second.md']],
  ['unreadable_file', [U + '/000_locked.md']],
]) cases.recomputed.push({id, cwd, paths, want: JSON.parse(shorten(JSON.stringify(O.recomputed(expand(cwd), paths.map((p) => ({path: expand(p), sha256: 'stale'}))))))});
// --- the multi_agent_v2 reader and the open packet ------------------------------------------------------------
const configDir = mkdtempSync(resolve(out, 'codex-'));
mkdirSync(resolve(home, '.codex'), {recursive: true});
const setConfig = (config, where) => {
  process.env.CODEX_HOME = where === 'home' ? '' : configDir;
  rmSync(resolve(configDir, 'config.toml'), {force: true}); rmSync(resolve(home, '.codex', 'config.toml'), {force: true});
  if (config !== null) writeFileSync(resolve(where === 'home' ? resolve(home, '.codex') : configDir, 'config.toml'), config);
};
const V2 = '[features.multi_agent_v2]\nenabled = true\n', F = '[features]\n', T = '[features.multi_agent_v2]\n';
for (const [id, config, where = 'codex-home'] of [
  ['absent_config', null], ['empty_config', ''], ['table_true', V2], ['table_false', T + 'enabled = false\n'], ['table_no_enabled', T + 'other = true\n'],
  ['table_beats_scalar', T + 'enabled = false\n' + F + 'multi_agent_v2 = true\n'], ['table_absent_falls_to_scalar', F + 'multi_agent_v2 = true\n'],
  ['scalar_false', F + 'multi_agent_v2 = false\n'], ['scalar_with_comment', F + 'multi_agent_v2 = true # on\n'], ['scalar_indented', F + '    multi_agent_v2   =   true\n'],
  ['scalar_quoted_is_unread', F + 'multi_agent_v2 = "true"\n'], ['scalar_other_key', F + 'multi_agent_v2_x = true\n'], ['scalar_suffix_key', F + 'xmulti_agent_v2 = true\n'],
  ['inline_enabled', F + 'multi_agent_v2 = { enabled = true }\n'], ['inline_disabled', F + 'multi_agent_v2 = { enabled = false }\n'],
  ['inline_without_enabled', F + 'multi_agent_v2 = { other = 1 }\n'], ['inline_spans_lines', F + 'multi_agent_v2 = {\n  enabled = true\n}\n'],
  ['inline_unanchored_enabled', F + 'multi_agent_v2 = { xenabled = true }\n'], ['scalar_beats_inline', F + 'multi_agent_v2 = false\nmulti_agent_v2 = { enabled = true }\n'],
  ['features_ends_at_next_table', F + 'other = 1\n[other]\nmulti_agent_v2 = true\n'], ['v2_table_ends_at_next_table', T + 'other = 1\n[x]\nenabled = true\n'],
  ['header_comment', '[features.multi_agent_v2] # note\nenabled = true\n'], ['header_indented', '  ' + V2], ['header_spaced_inside_is_unread', '[ features.multi_agent_v2 ]\nenabled = true\n'],
  ['header_dotted_wildcard', '[featuresXmulti_agent_v2]\nenabled = true\n'], ['header_array_of_tables', '[[features.multi_agent_v2]]\nenabled = true\n'],
  ['header_trailing_junk', '[features.multi_agent_v2] x\nenabled = true\n'], ['first_header_wins', T + 'enabled = false\n' + V2],
  ['first_enabled_wins', T + 'enabled = false\nenabled = true\n'], ['commented_header', '# ' + V2], ['commented_key', T + '# enabled = true\n'],
  ['key_prefixed_by_text', T + 'xenabled = true\n'], ['value_with_trailing_text', T + 'enabled = true x\n'], ['crlf_table', V2.replace(/\n/g, '\r\n')],
  ['crlf_scalar', '[features]\r\nmulti_agent_v2 = true\r\n'], ['crlf_inline', '[features]\r\nmulti_agent_v2 = { enabled = true }\r\n'],
  ['lone_cr_line_break', T.slice(0, -1) + '\renabled = true\r'], ['lone_cr_inside_header_line', T.slice(0, -1) + '\r# x\nenabled = true\n'],
  ['lone_cr_in_comment', T + 'enabled = true # a\rb\n'], ['lone_cr_between_keys', T + 'other = 1\renabled = true\n'],
  ['line_separator_between_keys', T + 'other = 1\u2028enabled = true\n'], ['paragraph_separator_in_comment', T + 'enabled = true # a\u2029b\n'],
  ['line_separator_header', T.slice(0, -1) + '\u2028\nenabled = true\n'], ['crcr_lf', T.slice(0, -1) + '\r\r\nenabled = true\r\r\n'],
  ['crcr_lf_comment', T + 'enabled = true # comment\r\r\n'], ['crcr_lf_scalar_comment', F + 'multi_agent_v2 = true # comment\r\r\n'],
  ['crcr_lf_inline', F + 'multi_agent_v2 = { enabled = true }\r\r\n'], ['header_comment_crcr_lf', '[features.multi_agent_v2] # note\r\r\nenabled = true\n'],
  ['header_comment_cr_only', '[features.multi_agent_v2] # note\renabled = true\n'], ['vertical_tab_indent', T + '\u000benabled = true\n'],
  ['nbsp_indent', T + '\u00a0enabled = true\n'], ['ideographic_space', T + '\u3000enabled\u3000=\u3000true\u3000\n'],
  ['zero_width_space_is_not_space', T + '\u200benabled = true\n'], ['bom_prefix', '\ufeff' + V2], ['bom_before_header', '\ufeff[features.multi_agent_v2]\nenabled = true\n'],
  ['value_split_across_lines', T + 'enabled =\ntrue\n'], ['tabs', T + '\tenabled\t=\ttrue\t\n'], ['replacement_char_in_comment', T + 'enabled = true # \ufffd\n'],
  ['no_trailing_newline', T + 'enabled = true'], ['empty_codex_home_uses_home', V2, 'home'], ['empty_codex_home_absent', null, 'home'],
]) { setConfig(config, where); cases.toml.push({id, config, where, want: O.v2SpawnSurface()}); }
for (const [id, config, launchId, roundId, fileCount] of [
  ['v2_on', V2, 'r1-20261004101010', 'r1', 1], ['v2_off', T + 'enabled = false\n', 'r1-20261004101010', 'r1', 1], ['no_config', null, 'r12-20261231235959', 'r12', 3],
  ['zero_files', V2, 'r2-20261004101010', 'r2', 0], ['empty_ids', null, '', '', 2], ['ids_with_text', null, 'LAUNCH: x', 'a b', 7],
]) { setConfig(config, 'codex-home'); cases.packet.push({id, config, launchId, roundId, fileCount, want: O.renderOpenPacket({roundId, lane: {launchId}}, fileCount)}); }
rmSync(probe);
writeFileSync(resolve(out, 'oracle.json'), JSON.stringify({oracle: 'CXC v0.2.40 3c1459ac', world, ...cases}, null, 2) + '\n');
for (const p of Object.keys(world.modes)) chmodSync(resolve(root, p), 0o644);
for (const dir of [root, configDir, home]) rmSync(dir, {recursive: true, force: true});
console.log('recorded ' + Object.entries(cases).map(([k, v]) => k + '=' + (Array.isArray(v) ? v.length : 1)).join(' '));
