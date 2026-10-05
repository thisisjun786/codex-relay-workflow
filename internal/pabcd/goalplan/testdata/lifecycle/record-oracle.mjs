// record-oracle.mjs — records the CXC v0.2.40 answers for the goalplan lifecycle operations
// (goalplan.ts:1236-1445, commit 3c1459ac) into testdata/lifecycle/oracle.json.
// Run by hand against the extracted read-only oracle build; no Go or production code reads it.
// usage: ORACLE_ROOT=<component dir> node record-oracle.mjs [--out <dir|file>]
import { writeFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { join } from 'node:path';

const oracleRoot = process.env.ORACLE_ROOT || '/var/tmp/cxc-v0.2.40/plugins/codexclaw/components/pabcd-state';
const outIdx = process.argv.indexOf('--out');
const outArg = outIdx >= 0 ? process.argv[outIdx + 1] : undefined;
const out = outArg && outArg !== '--out' ? outArg : join(oracleRoot, 'lifecycle-oracle.json');
const mod = await import(pathToFileURL(join(oracleRoot, 'dist', 'goalplan.js')).href);
const { askGoalplanDecision, decideGoalplanDecision, addGoalplanTask, completeGoalplanTask, meetGoalplanCriterion, unmetCriteria, doneWorkPhasesWithPendingTasks, isGoalplanComplete } = mod;

const projectPlan = (plan) => ({
  workPhases: plan.workPhases.map((wp) => ({
    id: wp.id, status: wp.status,
    awaitsDecision: wp.awaitsDecision === undefined ? null : wp.awaitsDecision,
    tasks: wp.tasks.map((t) => ({ id: t.id, status: t.status, outcome: t.outcome === undefined ? null : t.outcome, dependsOn: t.dependsOn === undefined ? null : t.dependsOn })),
  })),
  criteria: plan.criteria.map((c) => ({ id: c.id, status: c.status, capturedEvidence: c.capturedEvidence === undefined ? null : c.capturedEvidence })),
  decisions: (plan.decisions === undefined ? [] : plan.decisions).map((d) => ({ id: d.id, question: d.question, status: d.status, answer: d.answer === undefined ? null : d.answer, askedAt: d.askedAt, decidedAt: d.decidedAt === undefined ? null : d.decidedAt, recommendation: d.recommendation === undefined ? null : d.recommendation, options: d.options === undefined ? null : d.options })),
});
const observed = (res) => ({ kind: res.kind, reason: res.reason === undefined ? null : res.reason, plan: res.plan === undefined ? null : projectPlan(res.plan) });

// --- plan fixtures ---
const ts = '2026-01-01T00:00:00.000Z';
const decision = (id, question, extra) => Object.assign({ id: id, question: question, status: 'open', askedAt: '2026-01-01T00:00:00.000Z' }, extra === undefined ? {} : extra);
const task = (id, title, extra) => Object.assign({ id: id, title: title, status: 'pending' }, extra === undefined ? {} : extra);
const phase = (id, title, status, tasks, extra) => Object.assign({ id: id, title: title, status: status, tasks: tasks, criteriaIds: [] }, extra === undefined ? {} : extra);
const plan = (phases, extra) => Object.assign({
  objective: 'Ship the export feature', slug: 'ship-the-export-feature', createdAt: ts, updatedAt: ts, activeWorkPhaseId: null,
  workPhases: phases, criteria: [{ id: 'c-1', scenario: 'CSV export works', expectedEvidence: '', capturedEvidence: null, status: 'open' }],
}, extra === undefined ? {} : extra);

const openBase = () => plan([ phase('wp1', 'Exporter', 'in_progress', [ task('t-1', 'Write the CSV writer'), task('t-2', 'Wire the CLI', { dependsOn: ['t-1'] }) ]) ]);
const withDecision = () => plan([ phase('wp1', 'Exporter', 'pending', [], { awaitsDecision: ['dec-1'] }) ], { decisions: [ decision('dec-1', 'Choose API') ] });
const decided = (answer) => plan([ phase('wp1', 'Exporter', 'pending', [], { awaitsDecision: ['dec-1'] }) ], { decisions: [ decision('dec-1', 'Choose API', { status: 'decided', answer: answer, decidedAt: '2026-01-02T00:00:00.000Z' }) ] });

const cases = [];
const add = (name, fn, buildPlan, args, extra) => cases.push(Object.assign({ name: name, fn: fn, plan: buildPlan(), args: args }, extra === undefined ? {} : extra));

// ask
add('ask_ok_minimal', 'ask', openBase, { id: 'dec-1', question: 'Choose API', workPhaseIds: ['wp1'], askedAt: ts });
add('ask_ok_options_and_recommendation', 'ask', openBase, { id: 'dec-1', question: 'Choose API', recommendation: ' A ', options: [' A ', 'B'], workPhaseIds: ['wp1'], askedAt: ts });
add('ask_ok_options_without_recommendation', 'ask', openBase, { id: 'dec-1', question: 'Choose API', options: ['A', 'B'], workPhaseIds: [], askedAt: ts });
add('ask_ok_no_options_key', 'ask', openBase, { id: 'dec-1', question: 'Choose API', workPhaseIds: [], askedAt: ts });
add('ask_reject_uppercase_id', 'ask', openBase, { id: 'Dec-1', question: 'Q', workPhaseIds: [], askedAt: ts });
add('ask_reject_empty_id', 'ask', openBase, { id: '   ', question: 'Q', workPhaseIds: [], askedAt: ts });
add('ask_reject_overlong_id', 'ask', openBase, { id: 'a'.repeat(41), question: 'Q', workPhaseIds: [], askedAt: ts });
add('ask_ok_40_char_id', 'ask', openBase, { id: 'a'.repeat(40), question: 'Q', workPhaseIds: [], askedAt: ts });
add('ask_reject_empty_question', 'ask', openBase, { id: 'dec-1', question: '  ', workPhaseIds: [], askedAt: ts });
add('ask_reject_empty_recommendation', 'ask', openBase, { id: 'dec-1', question: 'Q', recommendation: ' ', workPhaseIds: [], askedAt: ts });
add('ask_reject_empty_options', 'ask', openBase, { id: 'dec-1', question: 'Q', options: [], workPhaseIds: [], askedAt: ts });
add('ask_reject_blank_option', 'ask', openBase, { id: 'dec-1', question: 'Q', options: ['A', ' '], workPhaseIds: [], askedAt: ts });
add('ask_reject_duplicate_option', 'ask', openBase, { id: 'dec-1', question: 'Q', options: ['A', ' A'], workPhaseIds: [], askedAt: ts });
add('ask_reject_recommendation_not_in_options', 'ask', openBase, { id: 'dec-1', question: 'Q', recommendation: 'C', options: ['A', 'B'], workPhaseIds: [], askedAt: ts });
add('ask_reject_bad_asked_at_month', 'ask', openBase, { id: 'dec-1', question: 'Q', workPhaseIds: [], askedAt: '2026-13-01T00:00:00.000Z' });
add('ask_reject_bad_asked_at_shape', 'ask', openBase, { id: 'dec-1', question: 'Q', workPhaseIds: [], askedAt: '2026-01-01' });
add('ask_reject_duplicate_decision_id', 'ask', () => plan([ phase('wp1', 'Exporter', 'pending', []) ], { decisions: [ decision('dec-1', 'Choose API') ] }), { id: 'dec-1', question: 'Other', workPhaseIds: [], askedAt: ts });
add('ask_reject_same_open_question', 'ask', () => plan([ phase('wp1', 'Exporter', 'pending', []) ], { decisions: [ decision('dec-1', 'Choose API') ] }), { id: 'dec-2', question: ' Choose API ', workPhaseIds: [], askedAt: ts });
add('ask_ok_same_question_already_decided', 'ask', () => plan([ phase('wp1', 'Exporter', 'pending', []) ], { decisions: [ decision('dec-1', 'Choose API', { status: 'decided', answer: 'A', decidedAt: '2026-01-02T00:00:00.000Z' }) ] }), { id: 'dec-2', question: 'Choose API', workPhaseIds: [], askedAt: ts });
add('ask_reject_blank_work_phase_id', 'ask', openBase, { id: 'dec-1', question: 'Q', workPhaseIds: ['wp1', ' '], askedAt: ts });
add('ask_reject_duplicate_work_phase_ids', 'ask', openBase, { id: 'dec-1', question: 'Q', workPhaseIds: ['wp1', 'wp1'], askedAt: ts });
add('ask_reject_unknown_work_phase', 'ask', openBase, { id: 'dec-1', question: 'Q', workPhaseIds: ['ghost'], askedAt: ts });
add('ask_reject_done_work_phase', 'ask', () => plan([ phase('wp1', 'Exporter', 'done', []) ]), { id: 'dec-1', question: 'Q', workPhaseIds: ['wp1'], askedAt: ts });
add('ask_reject_superseded_work_phase', 'ask', () => plan([ phase('wp1', 'Exporter', 'superseded', [], { supersededBy: 'wp2' }), phase('wp2', 'Replacement', 'pending', []) ]), { id: 'dec-1', question: 'Q', workPhaseIds: ['wp1'], askedAt: ts });

// decide
add('decide_ok', 'decide', withDecision, { id: ' dec-1 ', answer: ' yes ', decidedAt: '2026-01-02T00:00:00.000Z' });
add('decide_reject_missing', 'decide', openBase, { id: 'dec-1', answer: 'yes', decidedAt: '2026-01-02T00:00:00.000Z' });
add('decide_reject_ambiguous', 'decide', () => plan([ phase('wp1', 'Exporter', 'pending', []) ], { decisions: [ decision('dec-1', 'First'), decision('dec-1', 'Second') ] }), { id: 'dec-1', answer: 'yes', decidedAt: '2026-01-02T00:00:00.000Z' });
add('decide_reject_empty_answer', 'decide', withDecision, { id: 'dec-1', answer: '  ', decidedAt: '2026-01-02T00:00:00.000Z' });
add('decide_reject_bad_decided_at', 'decide', withDecision, { id: 'dec-1', answer: 'yes', decidedAt: 'yesterday' });
add('decide_unchanged_same_answer', 'decide', () => decided('yes'), { id: 'dec-1', answer: ' yes ', decidedAt: '2026-01-03T00:00:00.000Z' });
add('decide_reject_different_answer', 'decide', () => decided('yes'), { id: 'dec-1', answer: 'no', decidedAt: '2026-01-03T00:00:00.000Z' });

// add-task
add('add_ok_minimal', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-3', title: ' Add docs ' } });
add('add_ok_with_dependencies', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-3', title: 'Docs', dependsOn: [' t-1 '] } });
add('add_reject_bad_id', 'add', openBase, { workPhaseId: 'wp1', input: { id: 'T-3', title: 'Docs' } });
add('add_reject_empty_title', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-3', title: ' ' } });
add('add_reject_blank_dependency', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-3', title: 'Docs', dependsOn: [' '] } });
add('add_reject_duplicate_dependency', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-3', title: 'Docs', dependsOn: ['t-1', 't-1'] } });
add('add_reject_unknown_phase', 'add', openBase, { workPhaseId: 'ghost', input: { id: 't-3', title: 'Docs' } });
add('add_reject_done_phase', 'add', () => plan([ phase('wp1', 'Exporter', 'done', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }) ]) ]), { workPhaseId: 'wp1', input: { id: 't-3', title: 'Docs' } });
add('add_reject_superseded_phase', 'add', () => plan([ phase('wp1', 'Exporter', 'superseded', [], { supersededBy: 'wp2' }), phase('wp2', 'Replacement', 'pending', []) ]), { workPhaseId: 'wp1', input: { id: 't-3', title: 'Docs' } });
add('add_reject_duplicate_task', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-1', title: 'Docs' } });
add('add_reject_unknown_dependency_integrity', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-3', title: 'Docs', dependsOn: ['ghost'] } });
add('add_reject_cycle_integrity', 'add', openBase, { workPhaseId: 'wp1', input: { id: 't-1', title: 'x', dependsOn: ['t-2'] } });

// complete-task
add('complete_ok', 'complete', openBase, { workPhaseId: 'wp1', taskId: 't-1', outcome: ' writer verified by 4 tests ' });
add('complete_reject_empty_outcome', 'complete', openBase, { workPhaseId: 'wp1', taskId: 't-1', outcome: ' ' });
add('complete_reject_missing_task', 'complete', openBase, { workPhaseId: 'wp1', taskId: 'ghost', outcome: 'ok' });
add('complete_unchanged_already_done', 'complete', () => plan([ phase('wp1', 'Exporter', 'in_progress', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }) ]) ]), { workPhaseId: 'wp1', taskId: 't-1', outcome: 'again' });
add('complete_reject_not_ready_task_dependency', 'complete', openBase, { workPhaseId: 'wp1', taskId: 't-2', outcome: 'ok' });
add('complete_ok_task_dependency_done', 'complete', () => plan([ phase('wp1', 'Exporter', 'in_progress', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }), task('t-2', 'b', { dependsOn: ['t-1'] }) ]) ]), { workPhaseId: 'wp1', taskId: 't-2', outcome: 'ok' });
add('complete_reject_phase_dependency_pending', 'complete', () => plan([ phase('wp1', 'Exporter', 'pending', []), phase('wp2', 'Docs', 'pending', [ task('t-1', 'a') ], { dependsOn: ['wp1'] }) ]), { workPhaseId: 'wp2', taskId: 't-1', outcome: 'ok' });
add('complete_reject_phase_awaits_decision', 'complete', withDecision, { workPhaseId: 'wp1', taskId: 't-1', outcome: 'ok' });
add('complete_reject_blocked_phase', 'complete', () => plan([ phase('wp1', 'Exporter', 'blocked', [ task('t-1', 'a') ], { blockedReason: 'waiting' }) ]), { workPhaseId: 'wp1', taskId: 't-1', outcome: 'ok' });

// meet-criterion
add('meet_ok', 'meet', openBase, { criterionId: 'c-1', evidence: ' 4 tests green ' });
add('meet_reject_empty_evidence', 'meet', openBase, { criterionId: 'c-1', evidence: ' ' });
add('meet_reject_missing', 'meet', openBase, { criterionId: 'c-2', evidence: 'ok' });
add('meet_unchanged_already_met', 'meet', () => plan([ phase('wp1', 'Exporter', 'in_progress', []) ], { criteria: [{ id: 'c-1', scenario: 's', expectedEvidence: '', capturedEvidence: 'done', status: 'met' }] }), { criterionId: 'c-1', evidence: 'again' });
add('meet_ok_duplicate_criterion_ids', 'meet', () => plan([ phase('wp1', 'Exporter', 'in_progress', []) ], { criteria: [{ id: 'c-1', scenario: 's', expectedEvidence: '', capturedEvidence: null, status: 'open' }, { id: 'c-1', scenario: 's2', expectedEvidence: '', capturedEvidence: null, status: 'open' }] }), { criterionId: 'c-1', evidence: 'ok' });

// derived helpers
add('unmet_mixed', 'unmet', () => plan([ phase('wp1', 'Exporter', 'in_progress', []) ], { criteria: [{ id: 'c-1', scenario: 's', expectedEvidence: '', capturedEvidence: null, status: 'open' }, { id: 'c-2', scenario: 's', expectedEvidence: '', capturedEvidence: 'done', status: 'met' }, { id: 'c-3', scenario: 's', expectedEvidence: '', capturedEvidence: null, status: 'open' }] }), {});
add('done_with_pending_found', 'doneWithPending', () => plan([ phase('wp1', 'Exporter', 'done', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }), task('t-2', 'b') ]) ]), {});
add('done_with_pending_empty', 'doneWithPending', () => plan([ phase('wp1', 'Exporter', 'done', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }) ]) ]), {});
add('is_complete_true', 'isComplete', () => plan([ phase('wp1', 'Exporter', 'done', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }) ]) ], { criteria: [{ id: 'c-1', scenario: 's', expectedEvidence: '', capturedEvidence: 'done', status: 'met' }] }), {});
add('is_complete_false_unmet_criterion', 'isComplete', () => plan([ phase('wp1', 'Exporter', 'done', [ task('t-1', 'a', { status: 'done', outcome: 'ok' }) ]) ]), {});
add('is_complete_false_done_phase_pending_task', 'isComplete', () => plan([ phase('wp1', 'Exporter', 'done', [ task('t-1', 'a') ]) ], { criteria: [{ id: 'c-1', scenario: 's', expectedEvidence: '', capturedEvidence: 'done', status: 'met' }] }), {});
add('is_complete_true_vacuous', 'isComplete', () => plan([], { criteria: [] }), {});

const recorded = cases.map((c) => {
  const p = structuredClone(c.plan);
  const input = projectPlan(p);
  let expect;
  let after = p;
  if (c.fn === 'ask') { const r = askGoalplanDecision(p, c.args); expect = observed(r); after = r.plan === undefined ? p : r.plan; }
  else if (c.fn === 'decide') { const r = decideGoalplanDecision(p, c.args.id, c.args.answer, c.args.decidedAt); expect = observed(r); after = r.plan === undefined ? p : r.plan; }
  else if (c.fn === 'add') { const r = addGoalplanTask(p, c.args.workPhaseId, c.args.input); expect = observed(r); after = r.plan === undefined ? p : r.plan; }
  else if (c.fn === 'complete') { const r = completeGoalplanTask(p, c.args.workPhaseId, c.args.taskId, c.args.outcome); expect = observed(r); after = r.plan === undefined ? p : r.plan; }
  else if (c.fn === 'meet') { const r = meetGoalplanCriterion(p, c.args.criterionId, c.args.evidence); expect = observed(r); after = r.plan === undefined ? p : r.plan; }
  else if (c.fn === 'unmet') { expect = { ids: unmetCriteria(p).map((x) => x.id) }; }
  else if (c.fn === 'doneWithPending') { expect = { ids: doneWorkPhasesWithPendingTasks(p).map((x) => x.id) }; }
  else if (c.fn === 'isComplete') { expect = { value: isGoalplanComplete(p) }; }
  else { throw new Error('unknown fn ' + c.fn); }
  return { name: c.name, fn: c.fn, plan: c.plan, args: c.args, input: input, expect: expect, after: projectPlan(after) };
});

const doc = { oracle: 'CXC v0.2.40 goalplan.ts:1236-1445 (commit 3c1459acadeb1906d97c00a598e1457327ae372d)', recordedWith: process.version, cases: recorded };
writeFileSync(out, JSON.stringify(doc, null, 2) + '\n');
console.log('recorded ' + recorded.length + ' cases to ' + out);
