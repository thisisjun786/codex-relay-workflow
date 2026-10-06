// CXC v0.2.40 3c1459ac. Runs the original cursor-bearing tests unchanged except
// import redirects into a recording wrapper, then records what the oracle's
// effectiveActiveWorkPhaseId answered for every plan those tests handed it, and the
// absentSuccessorDetail wording for each reason. Usage:
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
  `export const cases=[];export let current='';export function test(name,fn){current=name;fn();}export function capture(fn,args,expected){const plan=structuredClone(args[0]);plan.createdAt=plan.updatedAt='2026-01-01T00:00:00.000Z';cases.push({test:current,plan,expected:structuredClone(expected)});}`,
);
fs.writeFileSync(
  path.join(scratch, 'wrapper.mjs'),
  `export * from '${dist}/goalplan.js';import * as g from '${dist}/goalplan.js';import {capture} from './capture.mjs';\n` +
    `export function effectiveActiveWorkPhaseId(plan){const v=g.effectiveActiveWorkPhaseId(plan);capture('effectiveActiveWorkPhaseId',[plan],v);return v;}`,
);
for (const file of ['work-phase-states.test.ts', 'goalplan-regression.test.ts']) {
  let text = fs.readFileSync(path.join(root, 'test', file), 'utf8');
  text = text.replace(/^test\([\s\S]*?^\}\);/gm, (body) => (/effectiveActiveWorkPhaseId\(/.test(body) ? body : ''));
  text = text
    .replace('from "node:test"', 'from "./capture.mjs"')
    .replace('from "../src/goalplan.ts"', 'from "./wrapper.mjs"')
    .replace(/from "\.\.\/src\/([^".]+)\.ts"/g, (_, name) => `from "${dist}/${name}.js"`)
    .replace('from "../test-support/symlink-support.ts"', `from "${pathToFileURL(path.join(root, 'test-support/symlink-support.ts')).href}"`);
  text = text.replace(
    'join(here, "fixtures", "goalplans-pre-change-baseline.json")',
    JSON.stringify(path.join(root, 'test/fixtures/goalplans-pre-change-baseline.json')),
  );
  const target = path.join(scratch, file);
  fs.writeFileSync(target, text);
  await import(pathToFileURL(target).href);
}
const { cases } = await import(pathToFileURL(path.join(scratch, 'capture.mjs')).href);
const g = await import(dist + '/goalplan.js');
const effectiveCursor = cases.map((c) => ({ test: c.test, plan: c.plan, expected: c.expected }));
// Two edge cases the original tests do not reach: a JS-truthiness cursor of "" and a
// dangling cursor. The oracle treats "" as unset, so a phase that shares the empty id
// never wins the cursor branch.
const extraCursor = [
  ['empty-string cursor is unset and falls through', { schemaVersion: 1, activeWorkPhaseId: '', workPhases: [
    { id: 'real', title: 'real', status: 'in_progress', tasks: [], criteriaIds: [] },
    { id: '', title: 'ghost', status: 'pending', tasks: [], criteriaIds: [] },
  ] }],
  ['a dangling cursor falls through to the pending phase', { schemaVersion: 1, activeWorkPhaseId: 'ghost', workPhases: [
    { id: 'a', title: 'a', status: 'done', tasks: [], criteriaIds: [] },
    { id: 'b', title: 'b', status: 'pending', tasks: [], criteriaIds: [] },
  ] }],
];
for (const [test, over] of extraCursor) {
  const plan = { ...g.buildGoalplan({ objective: 'cursor edge', now: () => '2026-01-01T00:00:00.000Z' }), ...over };
  plan.createdAt = plan.updatedAt = '2026-01-01T00:00:00.000Z';
  effectiveCursor.push({ test, plan, expected: g.effectiveActiveWorkPhaseId(plan) });
}
const absentSuccessorDetail = ['absent', 'not_runnable', 'dependencies_unmet', 'unexpected'].map((reason) => ({
  reason,
  expected: g.absentSuccessorDetail(reason),
}));
process.stdout.write(JSON.stringify({ oracle: 'CXC v0.2.40 3c1459ac', effectiveCursor, absentSuccessorDetail }) + '\n');
