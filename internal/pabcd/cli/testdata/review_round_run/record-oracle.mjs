// CXC v0.2.40 (3c1459ac), pabcd-state/src/review-round-cli.ts:210-306 (runReviewRoundCli).
// Usage: node record-oracle.mjs <oracle dist directory> <output directory>
// Only the output directory is written; the oracle is imported read-only. Each case seeds a workspace with the oracle's own functions,
// snapshots it (timestamps pinned), runs the steps through the oracle's parser and runner and records the answer of every CLI step.
// A verdict step stands in for the SubagentStop observer: the oracle's recordVerdict is applied and the resulting goalplan is recorded
// as a file write, so the Go replay needs no observer. Output and goalplan text are masked the way review_round_run_test.go masks them.
import {mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync} from 'node:fs';
import {dirname, join, relative, resolve} from 'node:path';
import {pathToFileURL} from 'node:url';
const [dist, out] = process.argv.slice(2);
const load = (name) => import(pathToFileURL(resolve(dist, name + '.js')).href);
const [CLI, GP, ST, RR] = await Promise.all([load('review-round-cli'), load('goalplan'), load('state'), load('review-round')]);
mkdirSync(out, {recursive: true});
const home = mkdtempSync(join(out, 'home-'));
Object.assign(process.env, {HOME: home, CODEX_HOME: join(home, '.codex')});
const FIXED = '2026-01-01T00:00:00.000Z', UNIT = 'devlog/_plan/260815_probe', DOC = UNIT + '/000_plan.md', SLUG = 'review-binding-probe';
const mask = (s) => s.replace(/r(\d+)-\d{14}/g, 'r$1-<STAMP>').replace(/\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z/g, '<TS>');
const pin = (s) => s.replace(/\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z/g, FIXED);
const phase = (id, status, extra = {}) => ({id, title: id, status, tasks: [], criteriaIds: [], ...extra});
const round = (id, status, extra = {}) => { const {lane = {}, ...rest} = extra; return {roundId: id, purpose: 'plan_audit', planPath: UNIT, planSha256: 'x', status, lane: {launchId: id + '-20260101000000', ...lane}, openedAt: FIXED, ...rest}; };
const walk = (dir) => readdirSync(dir).flatMap((n) => n === '.git' ? [] : statSync(join(dir, n)).isDirectory() ? walk(join(dir, n)) : [join(dir, n)]);
const cases = [];
// seed: the session (phase A bound to the unit unless state says otherwise), the goalplan (null for none), extra workspace files, steps.
function recordCase(id, {session = 'rb', state = {}, plan = {}, docs = {}, steps}) {
  const root = mkdtempSync(join(out, 'world-')), cwd = join(root, 'ws');
  mkdirSync(cwd, {recursive: true});
  for (const [rel, text] of Object.entries({[DOC]: '# probe\n', ...docs})) { mkdirSync(dirname(join(cwd, rel)), {recursive: true}); writeFileSync(join(cwd, rel), text); }
  const slug = state.slug ?? SLUG;
  if (plan !== null) {
    const g = GP.buildGoalplan({objective: 'review binding'});
    g.slug = slug;
    g.workPhases = plan.phases ?? [phase('wp1', 'in_progress')];
    if (plan.cursor !== null) g.activeWorkPhaseId = plan.cursor ?? 'wp1';
    if (plan.rounds) g.reviewRounds = plan.rounds;
    if (plan.cursorRound) g.activePlanAuditRoundId = plan.cursorRound;
    GP.writeGoalplan(cwd, g);
  }
  ST.writeState(cwd, {...ST.defaultState(session), phase: 'A', slug, planUnit: UNIT, planEpoch: 'e-probe-1', flags: {interview: false, auditPassed: false, checkPassed: false}, ...state});
  const files = {};
  for (const f of walk(cwd)) files[relative(cwd, f)] = pin(readFileSync(f, 'utf8'));
  const goalplanPath = '.codexclaw/goalplans/' + slug + '/goalplan.json';
  const read = () => { try { return readFileSync(join(cwd, goalplanPath), 'utf8'); } catch { return null; } };
  const recorded = [];
  for (const step of steps) {
    if (step.write) { for (const [rel, text] of Object.entries(step.write)) { mkdirSync(dirname(join(cwd, rel)), {recursive: true}); writeFileSync(join(cwd, rel), text); } recorded.push({write: step.write}); continue; }
    if (step.remove) { for (const rel of step.remove) rmSync(join(cwd, rel)); recorded.push({remove: step.remove}); continue; }
    if (step.verdict) {
      const g = GP.readGoalplan(cwd, slug), r = RR.latestRound(g, 'plan_audit');
      const done = RR.recordVerdict(g, {purpose: 'plan_audit', roundId: r.roundId, launchId: r.lane.launchId, verdict: step.verdict, reviewerSession: 'rv', now: () => FIXED});
      if (done.kind !== 'ok') throw new Error(id + ': ' + done.kind);
      GP.writeGoalplan(cwd, done.plan);
      recorded.push({write: {[goalplanPath]: pin(read()).replace(/r(\d+)-\d{14}/g, 'r$1-20260101000000')}}); continue;
    }
    const parsed = CLI.parseReviewRoundCliArgs(step.argv, cwd);
    const res = 'error' in parsed ? {code: 1, output: parsed.error} : CLI.runReviewRoundCli(parsed);
    recorded.push({argv: step.argv, want: {code: res.code, output: mask(res.output.split(cwd).join('@@WS@@'))}});
  }
  const after = read();
  cases.push({id, files, steps: recorded, after: after === null ? null : JSON.parse(mask(after))});
  rmSync(root, {recursive: true, force: true});
}
const S = ['--session', 'rb'], O = ['open', ...S, '--plan-path', DOC], show = ['show', ...S], json = ['show', ...S, '--json'], abort = ['abort', ...S];
const a = (argv) => ({argv});
const two = {[UNIT + '/010_second.md']: '# second\r\nwith crlf\r\n', [UNIT + '/notes.md']: 'notes\n'};
const closed = (id, verdict, status) => round(id, status, {closedAt: FIXED, lane: {verdict}});
recordCase('open_ok_then_show', {steps: [a(O), a(json), a(show)]});
recordCase('open_json_flag_is_ignored', {steps: [a([...O, '--json'])]});
recordCase('open_two_files_sorted_and_deduplicated', {docs: two, steps: [a(['open', ...S, '--plan-path', UNIT + '/010_second.md', '--plan-path', DOC, '--plan-path', './' + DOC]), a(json)]});
recordCase('open_twice_supersedes', {steps: [a(O), a(O), a(show)]});
recordCase('open_abort_open', {steps: [a(O), a(abort), a(O), a(show)]});
recordCase('open_padded_session', {steps: [a(['open', '--session', ' rb\t', '--plan-path', DOC]), a(json)]});
recordCase('open_unsafe_session_id', {session: 'a/b', steps: [a(['open', '--session', 'a/b', '--plan-path', DOC]), a(['show', '--session', 'a/b'])]});
recordCase('open_refreshes_a_pending_round', {plan: {rounds: [round('r1', 'pending')], cursorRound: 'r1'}, steps: [a(O), a(json)]});
recordCase('open_refuses_a_stale_reused_round', {plan: {rounds: [round('r1', 'pending'), closed('r2', undefined, 'inconclusive')], cursorRound: 'r1'}, steps: [a(O)]});
recordCase('open_after_a_closed_round', {plan: {rounds: [closed('r1', 'fail', 'changes_requested')]}, steps: [a(O), a(json)]});
recordCase('open_not_at_A', {state: {phase: 'P'}, steps: [a(O)]});
recordCase('open_idle', {state: {phase: 'IDLE'}, steps: [a(O)]});
recordCase('open_no_slug', {state: {slug: ''}, plan: null, steps: [a(O)]});
recordCase('open_no_unit', {state: {planUnit: null}, steps: [a(O)]});
recordCase('open_no_epoch', {state: {planEpoch: null}, steps: [a(O)]});
recordCase('open_empty_epoch', {state: {planEpoch: ''}, steps: [a(O)]});
recordCase('open_no_plan_path', {steps: [a(['open', ...S])]});
recordCase('open_foreign_path', {docs: {'package.json': '{}\n'}, steps: [a(['open', ...S, '--plan-path', 'package.json'])]});
recordCase('open_not_a_numbered_document', {docs: two, steps: [a(['open', ...S, '--plan-path', UNIT + '/notes.md'])]});
recordCase('open_missing_document', {steps: [a(['open', ...S, '--plan-path', UNIT + '/050_missing.md'])]});
recordCase('open_path_in_another_unit', {steps: [a(['open', ...S, '--plan-path', 'devlog/_plan/other/000_plan.md'])]});
recordCase('open_no_active_work_phase', {plan: {phases: [phase('wp1', 'done')]}, steps: [a(O)]});
recordCase('open_cursor_names_a_done_phase', {plan: {phases: [phase('wp1', 'done'), phase('wp2', 'pending')], cursor: 'wp1'}, steps: [a(O), a(json)]});
recordCase('open_unmet_dependency_moves_to_the_dependency', {plan: {phases: [phase('wp1', 'pending'), phase('wp2', 'in_progress', {dependsOn: ['wp1']})], cursor: 'wp2'}, steps: [a(O), a(json)]});
recordCase('open_without_a_cursor_takes_in_progress', {plan: {phases: [phase('wp1', 'pending'), phase('wp2', 'in_progress')], cursor: null}, steps: [a(O), a(json)]});
recordCase('open_goalplan_missing', {plan: null, steps: [a(O)]});
recordCase('open_keeps_tasks_and_other_rounds', {plan: {phases: [phase('wp1', 'in_progress', {tasks: [{id: 't-1', title: 'first', status: 'done', outcome: 'ok'}, {id: 't-2', title: 'second', status: 'done', dependsOn: ['t-1'], outcome: 'ok too'}]})], rounds: [{...closed('r1', 'pass', 'approved'), purpose: 'final_gate'}]}, steps: [a(O), a(abort)]});
// The oracle's revival drops a stored round it cannot read and the write that follows deletes it; the Go port refuses (known-defects.md).
const unreadable = {roundId: 'r0', status: 'bogus'};
recordCase('open_drops_an_unreadable_round', {plan: {rounds: [closed('r1', undefined, 'inconclusive'), unreadable]}, steps: [a(O), a(json)]});
recordCase('abort_drops_an_unreadable_round', {plan: {rounds: [round('r1', 'in_flight'), unreadable], cursorRound: 'r1'}, steps: [a(abort), a(json)]});
recordCase('abort_after_open', {steps: [a(O), a([...abort, '--reason', 'reviewer died']), a(show)]});
recordCase('abort_default_reason', {steps: [a(O), a(abort)]});
recordCase('abort_empty_reason', {steps: [a(O), a([...abort, '--reason', ''])]});
recordCase('abort_keeps_an_existing_reviewer_session', {plan: {rounds: [round('r1', 'in_flight', {lane: {reviewerSession: 'seeded'}})], cursorRound: 'r1'}, steps: [a([...abort, '--reason', 'late'])]});
recordCase('abort_no_round', {steps: [a(abort)]});
recordCase('abort_twice', {steps: [a(O), a(abort), a(abort)]});
recordCase('abort_no_slug', {state: {slug: ''}, plan: null, steps: [a(abort)]});
recordCase('abort_goalplan_missing', {plan: null, steps: [a(abort)]});
recordCase('show_no_round', {steps: [a(show), a(json)]});
recordCase('show_no_slug', {state: {slug: ''}, plan: null, steps: [a(show)]});
recordCase('show_goalplan_missing', {plan: null, steps: [a(show)]});
recordCase('show_stale_after_an_edit', {steps: [a(O), a(abort), a(json), {write: {[DOC]: '# probe amended\n'}}, a(json)]});
recordCase('show_open_round_is_never_stale', {steps: [a(O), {write: {[DOC]: '# probe amended\n'}}, a(json)]});
recordCase('show_removed_file_is_stale', {steps: [a(O), a(abort), {remove: [DOC]}, a(json)]});
recordCase('show_recorded_fail_verdict', {steps: [a(O), {verdict: 'fail'}, a(json), a(show)]});
recordCase('show_recorded_pass_then_edit', {steps: [a(O), {verdict: 'pass'}, a(show), {write: {[DOC]: '# changed\n'}}, a(show)]});
recordCase('show_pre_binding_round', {plan: {rounds: [closed('r1', 'near-pass', 'approved')]}, steps: [a(show), a(json)]});
recordCase('show_awkward_ids_in_json', {plan: {phases: [phase('wp<&>\u00e9\u2028', 'in_progress')], cursor: 'wp<&>\u00e9\u2028'}, state: {planEpoch: 'e<&>\u2029'}, steps: [a(O), a(json)]});
recordCase('show_latest_of_many_rounds', {plan: {rounds: [closed('r2', undefined, 'inconclusive'), closed('r10', 'pass', 'approved'), closed('r9', undefined, 'inconclusive')]}, steps: [a(show)]});
recordCase('help_words', {steps: [a(['help']), a([]), a(['open', '--help'])]});
recordCase('blank_session', {steps: [a(['show', '--session', ' \u00a0\ufeff ']), a(['show'])]});
writeFileSync(join(out, 'oracle.json'), JSON.stringify({oracle: 'CXC v0.2.40 3c1459ac', cases}, null, 1).replaceAll('@@WS@@', '$' + '{WS}') + '\n');
rmSync(home, {recursive: true, force: true});
console.log('recorded ' + cases.length + ' cases');
