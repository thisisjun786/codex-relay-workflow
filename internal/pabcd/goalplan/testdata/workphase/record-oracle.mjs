// CXC v0.2.40 3c1459ac. Runs the original work-phase-state tests unchanged except
// import redirects into a recording wrapper, then records what the oracle's
// closeFixedWorkPhase, resumeAbsentTarget and advanceWorkPhase answered for every plan
// and recordedNext those tests handed them. Usage:
//   node record-oracle.mjs <read-only source oracle root> <read-only recording oracle root> <scratch>
// stdout is generated testdata; no oracle write and no Node at Go test time.
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const [source, recording, scratch] = process.argv.slice(2);
const root = path.join(source, 'plugins/codexclaw/components/pabcd-state');
const dist = pathToFileURL(path.join(recording, 'plugins/codexclaw/components/pabcd-state/dist')).href;
fs.mkdirSync(scratch, { recursive: true });
process.env.TMPDIR = path.join(scratch, 'tmp');
fs.mkdirSync(process.env.TMPDIR, { recursive: true });

fs.writeFileSync(
  path.join(scratch, 'capture.mjs'),
  [
    'export const cases = [];',
    'export let current = "";',
    'export function test(name, fn) { current = name; fn(); }',
    // The projection both sides compare: the answer's kind and the fields that kind
    // carries, plus a plan reduced to the fields a close, a resume or an advance writes.
    'function projectPlan(plan) {',
    '  return {',
    '    activeWorkPhaseId: plan.activeWorkPhaseId ?? null,',
    '    workPhases: plan.workPhases.map((wp) => ({',
    '      id: wp.id, status: wp.status, dependsOn: wp.dependsOn ?? [],',
    '      tasks: wp.tasks.map((t) => ({ id: t.id, status: t.status })),',
    '    })),',
    '  };',
    '}',
    'export function project(result) {',
    '  const out = { kind: result.kind };',
    '  if ("closedId" in result) out.closedId = result.closedId ?? null;',
    '  if ("workPhaseId" in result) out.workPhaseId = result.workPhaseId;',
    '  if ("status" in result) out.status = result.status;',
    '  if ("successorId" in result) out.successorId = result.successorId;',
    '  if ("reason" in result) out.reason = result.reason;',
    '  if ("unmet" in result) out.unmet = result.unmet;',
    '  if ("pending" in result) out.pending = result.pending.map((t) => t.id);',
    '  if ("plan" in result && result.plan) out.plan = projectPlan(result.plan);',
    '  return out;',
    '}',
    'export function capture(entry) {',
    '  const input = structuredClone(entry.plan);',
    '  input.createdAt = input.updatedAt = "2026-01-01T00:00:00.000Z";',
    '  cases.push({ fn: entry.fn, test: current, target: entry.target ?? null, plan: input,',
    '    recordedNext: entry.recordedNext, expected: project(structuredClone(entry.result)) });',
    '}',
  ].join('\n'),
);

fs.writeFileSync(
  path.join(scratch, 'wrapper.mjs'),
  [
    'export * from "' + dist + '/goalplan.js";',
    'import * as g from "' + dist + '/goalplan.js";',
    'import { capture } from "./capture.mjs";',
    'export function closeFixedWorkPhase(plan, workPhaseId, recordedNext) {',
    '  const passed = arguments.length >= 3;',
    '  const result = passed ? g.closeFixedWorkPhase(plan, workPhaseId, recordedNext) : g.closeFixedWorkPhase(plan, workPhaseId);',
    '  capture({ fn: "closeFixedWorkPhase", plan, target: workPhaseId,',
    '    recordedNext: { present: passed, value: passed ? (recordedNext ?? null) : null }, result });',
    '  return result;',
    '}',
    'export function resumeAbsentTarget(plan, recordedNext) {',
    '  const result = g.resumeAbsentTarget(plan, recordedNext);',
    '  capture({ fn: "resumeAbsentTarget", plan, target: null,',
    '    recordedNext: { present: true, value: recordedNext ?? null }, result });',
    '  return result;',
    '}',
    'export function advanceWorkPhase(plan) {',
    '  const result = g.advanceWorkPhase(plan);',
    '  capture({ fn: "advanceWorkPhase", plan, target: null, recordedNext: { present: false, value: null }, result });',
    '  return result;',
    '}',
  ].join('\n'),
);

// newAdvanceSummary/oldAdvanceSummary are kept too: goalplan-regression.test.ts:184-191
// compares advanceWorkPhase's summary against the pre-wp4 one inside those helpers.
const target = /closeFixedWorkPhase|resumeAbsentTarget|advanceWorkPhase|newAdvanceSummary|oldAdvanceSummary/;
for (const file of ['work-phase-states.test.ts', 'goalplan-regression.test.ts', 'goalplan-concurrency.test.ts']) {
  let text = fs.readFileSync(path.join(root, 'test', file), 'utf8');
  text = text.replace(/^test\([\s\S]*?^\}\);/gm, (body) => (target.test(body) ? body : ''));
  text = text
    .replace('from "node:test"', 'from "./capture.mjs"')
    .replace('from "../src/goalplan.ts"', 'from "./wrapper.mjs"')
    .replace(/from "\.\.\/src\/([^".]+)\.ts"/g, (_, name) => 'from "' + dist + '/' + name + '.js"')
    .replace('from "../test-support/symlink-support.ts"', 'from "' + pathToFileURL(path.join(root, 'test-support/symlink-support.ts')).href + '"');
  text = text.replace(
    'join(here, "fixtures", "goalplans-pre-change-baseline.json")',
    JSON.stringify(path.join(root, 'test/fixtures/goalplans-pre-change-baseline.json')),
  );
  const out = path.join(scratch, file);
  fs.writeFileSync(out, text);
  await import(pathToFileURL(out).href);
}

const { cases, project } = await import(pathToFileURL(path.join(scratch, 'capture.mjs')).href);
const pick = (fn) => cases.filter((c) => c.fn === fn);
const g = await import(dist + '/goalplan.js');

// Branches no oracle test reaches. They are recorded from the same oracle build rather
// than asserted from the port's own reading of the source, so parity is measured and not
// argued: the null retry (a phase added after the first close must not start), the two
// corrupt markers, the settled done-successor normalisation, and every refusal variant.
const phase = (id, status, over = {}) => ({ id, title: 'phase ' + id, status, tasks: [], criteriaIds: [], ...over });
const planOf = (workPhases, over = {}) => {
  const p = g.buildGoalplan({ objective: 'extra', now: () => '2026-01-01T00:00:00.000Z' });
  p.schemaVersion = 1;
  p.workPhases = workPhases;
  Object.assign(p, over);
  return p;
};
const openDecision = { id: 'dec-1', question: 'Choose', status: 'open', askedAt: '2026-09-28T00:00:00.000Z' };
const absent = { present: false, value: null };
const recorded = (value) => ({ present: true, value });

// [test, plan, target, recordedNext]
const extraClose = [
  ['null retry does not start a phase added after the first close', planOf([phase('wp-1', 'in_progress'), phase('wp-2', 'pending')], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded(null)],
  ['a corrupt empty recorded successor is refused', planOf([phase('wp-1', 'in_progress')], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded('')],
  ['a corrupt self-successor is refused', planOf([phase('wp-1', 'in_progress')], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded('wp-1')],
  ['a recorded successor that is gone is refused', planOf([phase('wp-1', 'in_progress')], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded('ghost')],
  ['a recorded successor that is blocked is refused', planOf([phase('wp-1', 'in_progress'), phase('wp-2', 'blocked', { blockedReason: 'x' })], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded('wp-2')],
  ['a recorded successor waiting on a dependency is refused', planOf([phase('wp-1', 'in_progress'), phase('wp-2', 'pending', { dependsOn: ['wp-9'] })], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded('wp-2')],
  ['a finished successor leaves a cursor on the target cleared', planOf([phase('wp-1', 'in_progress'), phase('wp-2', 'done')], { activeWorkPhaseId: 'wp-1' }), 'wp-1', recorded('wp-2')],
  ['a finished successor keeps a cursor on a different running phase', planOf([phase('wp-1', 'in_progress'), phase('wp-2', 'done'), phase('wp-3', 'in_progress')], { activeWorkPhaseId: 'wp-3' }), 'wp-1', recorded('wp-2')],
  ['an already settled close answers already_done', planOf([phase('wp-1', 'done'), phase('wp-2', 'in_progress')], { activeWorkPhaseId: 'wp-2' }), 'wp-1', recorded('wp-2')],
  ['a close that holds an open task is refused', planOf([phase('wp-1', 'in_progress', { tasks: [{ id: 't1', title: 't', status: 'pending' }] })], { activeWorkPhaseId: 'wp-1' }), 'wp-1', absent],
  ['a missing target is absent', planOf([phase('wp-1', 'pending')]), 'ghost', absent],
  ['a blocked target is not runnable', planOf([phase('wp-1', 'blocked', { blockedReason: 'x' })]), 'wp-1', absent],
  ['a superseded target is not runnable', planOf([phase('wp-1', 'superseded', { supersededBy: 'wp-2' }), phase('wp-2', 'pending')]), 'wp-1', absent],
  ['a target with an unmet dependency is refused', planOf([phase('wp-1', 'in_progress', { dependsOn: ['wp-9'] })], { activeWorkPhaseId: 'wp-1' }), 'wp-1', absent],
  ['a target awaiting an open decision is refused', planOf([phase('wp-1', 'in_progress', { awaitsDecision: ['dec-1'] })], { activeWorkPhaseId: 'wp-1', decisions: [openDecision] }), 'wp-1', absent],
];
for (const [test, plan, target, next] of extraClose) {
  const result = next.present ? g.closeFixedWorkPhase(plan, target, next.value) : g.closeFixedWorkPhase(plan, target);
  const input = structuredClone(plan);
  input.createdAt = input.updatedAt = '2026-01-01T00:00:00.000Z';
  cases.push({ fn: 'closeFixedWorkPhase', test, target, plan: input, recordedNext: next, expected: project(result) });
}

const extraResume = [
  ['a marker with no successor cleans up', planOf([phase('wp-1', 'done')], { activeWorkPhaseId: null }), null],
  ['a successor that finished on its own cleans up', planOf([phase('wp-1', 'done'), phase('wp-2', 'done')], { activeWorkPhaseId: null }), 'wp-2'],
  ['a missing successor is refused', planOf([phase('wp-1', 'done')], { activeWorkPhaseId: null }), 'ghost'],
  ['a blocked successor is refused', planOf([phase('wp-1', 'done'), phase('wp-2', 'blocked', { blockedReason: 'x' })], { activeWorkPhaseId: null }), 'wp-2'],
  ['a successor waiting on a dependency is refused', planOf([phase('wp-1', 'done'), phase('wp-2', 'pending', { dependsOn: ['wp-9'] })], { activeWorkPhaseId: null }), 'wp-2'],
  ['a running successor with the matching cursor cleans up', planOf([phase('wp-1', 'done'), phase('wp-2', 'in_progress')], { activeWorkPhaseId: 'wp-2' }), 'wp-2'],
  ['a running successor with a moved cursor is reactivated', planOf([phase('wp-1', 'done'), phase('wp-2', 'in_progress')], { activeWorkPhaseId: null }), 'wp-2'],
  ['a pending successor is activated', planOf([phase('wp-1', 'done'), phase('wp-2', 'pending')], { activeWorkPhaseId: null }), 'wp-2'],
];
for (const [test, plan, next] of extraResume) {
  const result = g.resumeAbsentTarget(plan, next);
  const input = structuredClone(plan);
  input.createdAt = input.updatedAt = '2026-01-01T00:00:00.000Z';
  cases.push({ fn: 'resumeAbsentTarget', test, target: null, plan: input, recordedNext: { present: true, value: next }, expected: project(result) });
}

const extraAdvance = [
  ['advance refuses a phase holding an open task', planOf([phase('wp-1', 'in_progress', { tasks: [{ id: 't1', title: 't', status: 'pending' }] })], { activeWorkPhaseId: 'wp-1' })],
  ['advance with no work phase answers no_active', planOf([])],
  ['advance of the last phase nulls the cursor', planOf([phase('wp-1', 'in_progress')], { activeWorkPhaseId: 'wp-1' })],
  ['advance wraps to an earlier pending phase', planOf([phase('wp-1', 'pending'), phase('wp-2', 'in_progress')], { activeWorkPhaseId: 'wp-2' })],
];
for (const [test, plan] of extraAdvance) {
  const result = g.advanceWorkPhase(plan);
  const input = structuredClone(plan);
  input.createdAt = input.updatedAt = '2026-01-01T00:00:00.000Z';
  cases.push({ fn: 'advanceWorkPhase', test, target: null, plan: input, recordedNext: { present: false, value: null }, expected: project(result) });
}

process.stdout.write(JSON.stringify({
  oracle: 'CXC v0.2.40 3c1459ac',
  closeFixed: pick('closeFixedWorkPhase'),
  resumeAbsent: pick('resumeAbsentTarget'),
  advance: pick('advanceWorkPhase'),
}) + '\n');
