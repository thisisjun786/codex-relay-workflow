// Development-only recorder for the CRW-382 loop CLI port (C1a).
// Usage: node record-oracle.mjs <oracle-root> <scratch-root> > oracle.json
// <oracle-root> is the extracted CXC v0.2.40 tree (read-only).
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const [oracle, scratch] = process.argv.slice(2);
const src = path.join(oracle, 'plugins/codexclaw/components/pabcd-state/src');
const cli = await import(pathToFileURL(path.join(src, 'goalplan-cli.ts')));
const goalplan = await import(pathToFileURL(path.join(src, 'goalplan.ts')));
const state = await import(pathToFileURL(path.join(src, 'state.ts')));

const WS = '<WS>';
const normalize = (text, cwd) => text.split(cwd).join(WS);
const tmp = () => fs.mkdtempSync(path.join(scratch, 'loop-ws-'));

const parseCases = [
  [], ['help'], ['--help'], ['-h'], ['HELP'], ['help', 'x'], ['nope'], ['NoPe'], [''],
  ['init'], ['INIT', '--objective', 'x'], ['init', '--objective', 'x'], ['init', '--objective=x'],
  ['init', '--objective', 'x', '--criterion', 'a', '--criterion', 'b'],
  ['init', '--objective', 'x', '--criterion'], ['init', '--objective', 'x', '--criterion='],
  ['init', '--objective', 'x', '--objective', 'y'],
  ['init', '--objective', 'x', '--surface', 'logic'],
  ['init', '--objective', 'x', '--bogus'], ['init', '--objective', 'x', '--bogus=1'],
  ['init', '--objective', 'x', '--json'], ['init', '--objective', 'x', '--cwd'],
  ['init', '--objective', 'x', '--session', 's1', '--schema-version', '2'],
  ['init', '--objective', 'x', '--schema-version', '3'],
  ['init', '--objective', 'x', '--schema-version', 'abc'],
  ['init', '--objective', 'x', '--schema-version', '0x10'],
  ['init', '--objective', 'x', '--schema-version', '0b11'],
  ['init', '--objective', 'x', '--schema-version', '0o17'],
  ['init', '--objective', 'x', '--schema-version', 'Infinity'],
  ['init', '--objective', 'x', '--schema-version', '-Infinity'],
  ['init', '--objective', 'x', '--schema-version', 'NaN'],
  ['init', '--objective', 'x', '--schema-version', ''],
  ['init', '--objective', 'x', '--schema-version', ' 5 '],
  ['init', '--objective', 'x', '--schema-version', '1e3'],
  ['init', '--objective', 'x', '--schema-version', '1_0'],
  ['init', '--objective', 'x', '--schema-version', '1e'],
  ['init', '--objective', 'x', '--schema-version', '.5'],
  ['init', '--objective', 'x', '--schema-version', '1.'],
  ['init', '--objective', 'x', '--schema-version', '+2'],
  ['init', '--objective', 'x', '--schema-version', '0x_10'],
  ['show', '--slug', 's'], ['show', '--slug'], ['show', '--slug='], ['show', '--slug', '--objective'],
  ['show', '--slug', 's', '--slug', 't'], ['show', '--json'], ['show', 'positional'], ['show', '--'],
  ['show', '--objective', 'My Objective'], ['show', '--session', 's1'], ['show', '--cwd', '/tmp'],
  ['validate', '--slug', 's'], ['ready', '--slug', 's', '--json'], ['ready', '--json=1'],
  ['steer', '--session', 's1', '--batch-json', '{}'], ['steer', '--batch-json', 'x'],
  ['add-criterion', '--session', 's1', '--criterion', 'c', '--surface', 'web'],
  ['add-criterion', '--session', 's1', '--criterion', 'c', '--surface'],
  ['add-criterion', '--session', 's1', '--criterion', 'c', '--surface='],
  ['add-criterion', '--session', 's1', '--criterion', 'c', '--presented', 'native'],
  ['add-criterion', '--session', 's1', '--criterion', 'c', '--presented'],
  ['add-work-phase', '--session', 's1', '--id', 'w', '--title', 't', '--depends-on', 'a', '--depends-on', 'b'],
  ['add-work-phase', '--session', 's1', '--id', 'w', '--title', 't', '--depends-on', 'a', '--depends-on', 'a'],
  ['add-work-phase', '--session', 's1', '--id', 'w', '--title', 't', '--depends-on', ''],
  ['add-work-phase', '--session', 's1', '--id', 'w', '--title', 't', '--depends-on'],
  ['add-task', '--session', 's1', '--work-phase', 'wp1', '--id', 't1', '--title', 'T'],
  ['complete-task', '--session', 's1', '--work-phase', 'wp1', '--id', 't1', '--outcome', 'o'],
  ['meet-criterion', '--session', 's1', '--id', 'c-1', '--evidence', 'e'],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--option', 'a', '--option', 'b'],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--option', 'a', '--option', 'a'],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--option', '  '],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--work-phase', 'wp1', '--work-phase', 'wp2'],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--work-phase', 'wp1', '--work-phase', 'wp1'],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--work-phase', '  '],
  ['decide', '--session', 's1', '--id', 'd1', '--answer', 'a'],
  ['\u0130NIT', '--objective', 'x'], ['HELP', 'x'], ['help', '--help', 'x'],
  ['init', '--objective', 'x', '--surface', 'logic', '--surface', 'web'],
  ['ready', '--json', '--json'], ['show', '--slug', 's', '--cwd'],
  ['ask', '--session', 's1', '--id', 'd1', '--question', 'q', '--work-phase', 'wp1', '--work-phase', 'wp1'],
  ['add-work-phase', '--session', 's1', '--id', 'w', '--title', 't', '--depends-on', 'a', '--bogus'],
];

const out = {
  source: 'CXC v0.2.40 3c1459ac goalplan-cli.ts',
  classification: 'identical (command names substituted at comparison)',
  parse: parseCases.map((argv) => ({ argv, result: cli.parseGoalplanCliArgs(argv, WS) })),
  help: { code: 0, output: cli.renderGoalplanHelp() },
  plans: [],
  show: [],
};

// Rendered plan, no lock: init returns renderPlan(plan) without a lock status.
{
  const cwd = tmp();
  const r = cli.runGoalplanCli({ verb: 'init', objective: 'Ship the export feature', criteria: [], cwd });
  const file = path.join(cwd, '.codexclaw/goalplans/ship-the-export-feature/goalplan.json');
  const plan = JSON.parse(fs.readFileSync(file, 'utf8').split(cwd).join(WS).replace(/\d{4}-\d\d-\d\dT[0-9:.]+Z/g, '<TS>'));
  out.plans.push({ name: 'init-empty', lock: false, plan, output: normalize(r.output, cwd), code: r.code });
}

// Rendered plan with work phases, criteria and an open decision; show adds an absent lock line.
const richPlan = {
  objective: 'Rich plan', slug: 'rich-plan',
  createdAt: '2026-01-01T00:00:00.000Z', updatedAt: '2026-01-01T00:00:00.000Z',
  activeWorkPhaseId: 'wp1',
  workPhases: [
    { id: 'wp1', title: 'Phase One', status: 'in_progress', tasks: [{ id: 't1', title: 'Do a thing', status: 'done', outcome: 'did it' }], criteriaIds: [], awaitsDecision: ['d1'] },
    { id: 'wp2', title: 'Phase Two', status: 'pending', tasks: [], criteriaIds: [] },
  ],
  criteria: [{ id: 'c-1', scenario: 'the thing works', expectedEvidence: '', capturedEvidence: null, status: 'open' }],
  host: { armed: false, armedAt: null, source: 'none' },
  decisions: [
    { id: 'd1', question: 'which way?', recommendation: 'a', options: ['a', 'b'], status: 'open', askedAt: '2026-01-01T00:00:00.000Z' },
    { id: 'd2', question: 'answered?', status: 'decided', answer: 'yes', askedAt: '2026-01-01T00:00:00.000Z', decidedAt: '2026-01-01T00:00:00.000Z' },
  ],
  schemaVersion: 1,
};
const bound = tmp();
goalplan.writeGoalplan(bound, richPlan);
state.writeState(bound, { ...state.defaultState('rec-s1'), slug: 'rich-plan' });
{
  const args = cli.parseGoalplanCliArgs(['show', '--slug', 'rich-plan'], bound);
  const r = cli.runGoalplanCli(args);
  out.plans.push({ name: 'show-rich', lock: true, plan: richPlan, output: normalize(r.output, bound), code: r.code });
}

// A held lock: show renders the present branch, whose age is normalized.
{
  const lockDir = path.join(bound, '.codexclaw/goalplans/rich-plan/.goalplan.lock');
  fs.mkdirSync(lockDir, { recursive: true });
  const args = cli.parseGoalplanCliArgs(['show', '--slug', 'rich-plan'], bound);
  const r = cli.runGoalplanCli(args);
  const output = normalize(r.output, bound).replace(/ageMs=[0-9.]+/, 'ageMs=<AGE>');
  out.plans.push({ name: 'show-locked', lock: true, present: true, plan: richPlan, output, code: r.code });
  fs.rmSync(lockDir, { recursive: true });
}

// show cases: resolveSlug sources, the required-args message, and read failures.
const empty = tmp();
const showCases = [
  { name: 'slug-absent', ws: 'empty', argv: ['show', '--slug', 'My Slug'] },
  { name: 'objective-absent', ws: 'empty', argv: ['show', '--objective', 'Ship It!'] },
  { name: 'slug-blank-derived', ws: 'empty', argv: ['show', '--slug', '!!!'] },
  { name: 'session-bound', ws: 'bound', argv: ['show', '--session', 'rec-s1'] },
  { name: 'session-unbound', ws: 'bound', argv: ['show', '--session', 'rec-s2'] },
  { name: 'none', ws: 'empty', argv: ['show'] },
  { name: 'read-absent', ws: 'empty', argv: ['show', '--slug', 'no-such-plan'] },
];
for (const c of showCases) {
  const cwd = c.ws === 'bound' ? bound : empty;
  const args = cli.parseGoalplanCliArgs(c.argv, cwd);
  const r = cli.runGoalplanCli(args);
  out.show.push({ name: c.name, ws: c.ws, argv: c.argv, output: normalize(r.output, cwd), code: r.code });
}

process.stdout.write(JSON.stringify(out, null, 2) + '\n');
