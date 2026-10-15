// crw:lib/packets: the packets the project lane sends - plain text, built from the journal's facts and nothing else, so the same
// facts give the same bytes (the digest of a packet is part of the input digest of its call, and a resumed lane must build the
// packet it built). Every function is pure.
//
// A change of a packet's text is therefore a change of the input digest of every call built from it (engine/digest.go): a resume
// over a journal recorded with the earlier text runs that call live again, and with it the rest of its path. The design audit
// packet is the case of CRW-1321: a run that recorded its design audit under the earlier text runs design-audit:r1 again on a
// resume, and crw.designLedger then refuses the first revision again (the ledger has r0 and adds r1 next), so the resumed run ends in
// that refusal. That resume supersedes the recorded audit and r0 with it, so the binary that started the run refuses the run
// afterwards as well: a run past its design audit is finished by the binary that started it only while no process of a binary with
// other audit text has resumed it, and is otherwise started anew. A run that had not reached its design audit is not affected. The
// open-question routing of CRW-1347 changed the design audit packet again (the AUTHORITY section and the answers, each with its
// route) and the answers crw.explore hands crw.exploreRecord (each with the reason, the scope and the notation of its route): a run
// that recorded its open questions under the earlier binary replays its crw.exploreRoute rows and its explorer calls as recorded
// (their arguments are the same), so the routes of its open questions stay the recorded ones, an inline route among them, and runs
// crw.exploreRecord live again and, after it, design-audit:r1, the one agent call that runs again; a run past its design audit then
// comes to the same refusal of crw.designLedger. CRW-1366 changed the answers again (each carries the origin of its route) and their
// route lines, and asks an authority question in its audit round: a run that recorded its open questions under the earlier binary
// runs crw.exploreRecord live again on a resume and, where the packet or the asks differ, the ask and design-audit:r1, and a run
// past its design audit comes to the same refusal of crw.designLedger. The
// role overlays (internal/flow/roles/*.md) are no packet: they are the developer instructions a thread starts with, part of no input
// digest, so a change of them re-runs nothing; a turn on a thread started earlier runs under the instructions that thread was
// started with, and a new thread or a fork takes the new text. TestResumeAfterPromptTextChange pins both.

// canon renders a value with its keys in sorted order, so the text does not depend on the order an object was built in.
export function canon(value) {
  if (value === null || typeof value !== 'object') return JSON.stringify(value === undefined ? null : value)
  if (Array.isArray(value)) return '[' + value.map(canon).join(',') + ']'
  return '{' + Object.keys(value).sort().map((k) => JSON.stringify(k) + ':' + canon(value[k])).join(',') + '}'
}

function list(items, empty) {
  if (!items || items.length === 0) return ['- ' + (empty || 'none')]
  return items.map((s) => '- ' + s)
}

// criteriaList labels the criteria of a plan c1, c2, ...: the ids a verdict answers by, one entry for each (ops.CriterionIDs).
function criteriaList(criteria) {
  return list((criteria || []).map((c, i) => 'c' + (i + 1) + ': ' + c))
}

function section(title, lines) {
  return [title + ':'].concat(lines).join('\n')
}

// notesSection renders what explorers found, to go into the next packet of the node.
function notesSection(notes) {
  if (!notes || notes.length === 0) return ''
  return section('FINDINGS OF THE EXPLORER (use them; do not ask again)', notes.map((n) => '- ' + n.question + '\n  ' + n.answer))
}

export function architectPacket(ms) {
  const nodes = ms.nodes.map((n) => n.id + ' (' + n.issue + '): ' + n.title + (n.after.length ? ' after ' + n.after.join(', ') : '') + (n.group ? ' [group ' + n.group + ']' : '') +
    '\n  regions: ' + (n.regions.length ? n.regions.join(', ') : 'undeclared') + '\n  criteria: ' + (n.criteria.length ? n.criteria.join(' | ') : 'none'))
  return [
    'ROLE: architect. Plan milestone ' + ms.id + ' of plan ' + ms.plan + ' (' + ms.title + ').',
    'Answer with DESIGN: the decisions, one plan for each issue node below (its size_fit, edit regions, approach, tests first and done condition), the open questions you cannot answer from what you read, and the risks.',
    section('NODES', nodes),
  ].join('\n\n')
}

// The executor contract and the authority boundary the design audit holds a DESIGN to (CRW-1321): the architect overlay
// (internal/flow/roles/architect.md) states the same two to the architect.
const EXECUTOR_CONTRACT = [
  '- the executor never commits: its edits stay uncommitted in its own directory, and it runs no branch-level git command;',
  '- the run commits the directory after the executor\'s turn, with the fixed message `crw <node> <call>` and the request id, so no done condition can ask for text in a commit message or a commit body;',
  '- the executor reports a RECEIPT (outcome, files, verification, summary, gap, research_question, plan_mismatch), which has no field for a commit, a commit message or the reason for an exception;',
  '- a done condition is a check that runs at the committed HEAD of the node or a state of the worktree; one that needs the executor to commit first, or a check that compares with HEAD before the run\'s commit exists (as `make gui` does through `crw-dev ci gui-drift`), cannot be carried out;',
  '- a generated asset is planned with its generation command and its verification command, the verification running at the committed HEAD.',
]
const AUTHORITY = [
  '- an exception to a rule of the repository (scope `authority:repo-rule`);',
  '- the record of another issue (scope `authority:other-record`);',
  '- a policy decision (scope `authority:policy`);',
  '- a prerequisite of the execution environment (scope `authority:environment`).',
  'A DESIGN that decides one of these, or whose plan depends on one answer to such a question, is a finding. The same question raised as an open question, with the plan resting on an assumption the DESIGN states, is not, also when it reaches this audit unanswered: an open question whose scope starts with the notation of its kind goes to no explorer and is asked of the orchestrator as a design_question escalation that does not stop the milestone, and an open question does not stop the run. Each answer below names the route its question took.',
]

// answerLines renders the answers of the open questions for the design audit: each question with its route and what it got
// (CRW-1347), so no question reaches the audit as an empty answer without its route.
function answerLines(answers) {
  return list((answers || []).map((a) => {
    const lines = [(a.id ? a.id + ': ' : '') + a.question, '  route: ' + routeLine(a)]
    if (a.answer) lines.push('  answer: ' + a.answer)
    if (a.findings) lines.push('  findings: ' + canon(a.findings))
    return lines.join('\n')
  }), 'no open question')
}

function routeLine(a) {
  switch (a.route) {
    case 'spawn':
      return 'an explorer answered it (call ' + (a.call || 'none') + ')'
    case 'reuse':
      return 'reused: ' + reuseLine(a.reason) + (a.origin ? ' (original route: ' + (a.origin === 'spawn' ? 'explorer' : a.origin) + ')' : '')
    case 'orchestrator':
      return 'asked of the orchestrator (' + (a.reason || 'design_question') + (a.authority ? ', ' + a.authority : '') + '): no answer in this audit; ' + askLine(a.reason) + ', the milestone goes on and the plan stands on the assumption the DESIGN states'
    default:
      return a.route + (a.reason ? ' (' + a.reason + ')' : '') + ': no answer'
  }
}

function reuseLine(reason) {
  switch (reason) {
    case 'same_question_in_round':
      return 'the same question as another of this DESIGN'
    case 'answered_by_orchestrator':
      return 'the answer the orchestrator gave in the design_question escalation of the run'
    default:
      return 'the answer an earlier question of the run got'
  }
}

// askLine says when the run asks an orchestrator question (CRW-1366): an authority question in the round that routed it, once, and
// carried by each later ask of the run until it is answered.
function askLine(reason) {
  switch (reason) {
    case 'out_of_authority':
      return 'the run raised it in the design_question escalation of this round'
    case 'asked_in_run':
      return 'the run raised it in an earlier design_question escalation and does not ask it again on its own (a later ask of the run carries it while no answer has come)'
    default:
      return 'the run raises it in the design_question escalation when the DESIGN is approved'
  }
}

// auditFindings are the findings of a design audit by their ids: r<round>.<index>, the index 1-based in the audit's findings (a ref
// alone is not unique). The design-fix packet names them by these ids, and the critic's PRIOR FINDINGS in the later rounds (CRW-1452).
export function auditFindings(round, findings) {
  return (findings || []).map((f, i) => ({ id: 'r' + round + '.' + (i + 1), ref: f.ref, severity: f.severity, finding: f.finding }))
}

function findingLine(f) {
  return f.id + ' [' + f.severity + '] ' + f.ref + ': ' + f.finding
}

// dispositionOf renders what the architect's design-fix answer reported of one finding: folded or rebutted with its reason, not
// reported when the answer named no disposition of it, and null when the answer had no dispositions at all.
function dispositionOf(f, dispositions) {
  if (dispositions === null || dispositions === undefined) return 'null (no dispositions reported)'
  const d = dispositions.find((x) => x.finding === f.id)
  return d ? d.disposition + ': ' + d.reason : 'not reported'
}

// priorLines are the findings of the earlier audit rounds, each with the architect's disposition of it, and the dispositions that
// name no finding of their round. prior is [{round, findings: auditFindings(round, ...), dispositions}], in round order.
function priorLines(prior) {
  const out = []
  for (const p of prior) {
    for (const f of p.findings) out.push('- ' + findingLine(f) + ' -> ' + dispositionOf(f, p.dispositions))
    for (const d of p.dispositions || []) {
      if (!p.findings.some((f) => f.id === d.finding)) out.push('- ' + d.finding + ' (no finding of round ' + p.round + ' has this id) -> ' + d.disposition + ': ' + d.reason)
    }
  }
  return out.length === 0 ? ['- none'] : out
}

// The critic's packet (CRW-1452): the DESIGN, the answers to its open questions, and from the second round on the findings of the
// earlier rounds with the architect's disposition of each. prior is the per-round history (priorLines); it is left out in round 1.
// context is a re-plan's {head, base_tip, landed} (CRW-1458): the critic's directory is then a read-only checkout of the head the
// re-plan judges, and the packet names it as the re-plan's architect turn has it (headBlock); the first DESIGN has none.
export function designAuditPacket(ms, cand, round, prior, context) {
  const at = context ? headBlock(context) : null
  return [
    'ROLE: critic. Audit the DESIGN of milestone ' + ms.id + ' (round ' + round + ') before any executor starts. You did not write it and are of another model family than the architect.',
    'Answer with DESIGN_AUDIT: approved only when every issue node has a plan an executor can carry out inside its edit regions, every done condition can be met under the executor contract below, the decisions are consistent with the plan, no decision of the DESIGN is outside the architect\'s authority below, and no P0 or P1 finding stands; otherwise needs_changes with the findings the architect must act on. Check every done condition against the executor contract and every decision against the authority boundary: a done condition the contract cannot meet and a decision outside the authority are each a P1 finding that names its node or decision.',
    section('EXECUTOR CONTRACT (every done condition must be met under it)', EXECUTOR_CONTRACT),
    section('OUT-OF-AUTHORITY DECISIONS (the architect raises these as open questions and does not decide them)', AUTHORITY),
    round > 1 && prior ? section('PRIOR FINDINGS (the findings of the earlier rounds by id, each with the architect\'s disposition: folded, rebutted, or not reported)', priorLines(prior)) +
      '\nJudge each prior finding: a folded one is resolved only if this DESIGN does fold it; a rebuttal you do not accept is raised again in your findings under the same ref, with the reason you reject it; a finding not reported, or of a round whose dispositions are null, is still open.' : '',
    at ? 'Your directory is a read-only checkout of the pinned head: the DESIGN re-plans the milestone at it.\n' + at.head : '',
    at ? at.landed : '',
    section('DESIGN', [canon(cand.design)]),
    section('ANSWERS TO THE OPEN QUESTIONS (each with its route and what it got)', answerLines(cand.answers)),
  ].filter(Boolean).join('\n\n')
}

// The design-fix turn of the architect's thread: the findings of audit round round by id, and the dispositions it answers with, one
// for each id (CRW-1452).
export function designFixPacket(audit, context, round) {
  const findings = auditFindings(round, audit.findings)
  const at = context ? headBlock(context) : null
  return [
    'ROLE: architect. The independent audit of your DESIGN found what follows. Answer with the DESIGN again, with every finding addressed, and with dispositions: one entry for each finding id below (' + findings.map((f) => f.id).join(', ') + '), folded when the DESIGN now folds the finding or rebutted when you keep the DESIGN as it is, each with its reason. The critic reads your dispositions in its next round.',
    section('FINDINGS', findings.map((f) => '- ' + findingLine(f))),
    audit.summary ? 'AUDIT SUMMARY: ' + audit.summary : '',
    at ? 'Your directory P is a read-only checkout of the pinned head: work only under the absolute path P.\n' + at.head : '',
    at ? at.landed : '',
  ].filter(Boolean).join('\n\n')
}

function landedLines(landed) {
  return list((landed || []).map((l) => l.node + ': ' + l.head), 'none landed')
}

// headBlock is what a packet of a call at a pinned head says of it (CRW-1458): head, the HEAD / DESIGN RAN AT / RANGE lines (the
// range from the head the DESIGN ran at), and landed, the LANDED HEADS section with the head every node landed with. context is
// {head, base_tip, landed}. The design-fix, re-plan, reflection and re-plan critic packets share it.
export function headBlock(context) {
  return {
    head: 'HEAD: ' + context.head + '\nDESIGN RAN AT: ' + (context.base_tip || 'unknown') + '\nRANGE: ' + (context.base_tip ? context.base_tip + '..' : '') + context.head,
    landed: section('LANDED HEADS', landedLines(context.landed)),
  }
}

function drivenDecisions(cur) {
  return list(((cur.design && cur.design.decisions) || []).map((d) => d.id + ': ' + d.change), 'none recorded')
}

// The REFLECTION turn (SP-ARCH-3): the next turn of the architect's thread, at the integrated head the final gate pinned. The packet
// names the head, the range from the head the DESIGN ran at, and the head every node landed with; the turn's directory is a detached
// read-only worktree of that head.
export function reflectionPacket(ms, cur, closed) {
  const at = headBlock({ head: closed.gate.head, base_tip: cur.base_tip, landed: cur.landed })
  return [
    'ROLE: architect. Reflection at the close of milestone ' + ms.id + ' (' + ms.title + '), on the thread of your DESIGN (revision ' + cur.revision + '). Your directory P is a read-only checkout of the integrated head: work only under the absolute path P.',
    at.head,
    section('DECISIONS OF THE DESIGN (map each to the files and criteria of the integrated range)', drivenDecisions(cur)),
    at.landed,
    'REVIEWER-FINAL ALIGNMENT: ' + (closed.final ? closed.final.alignment : 'none'),
    'Answer with REFLECTION: ALIGNED, or MISALIGNED with each decision the range departs from, what you observed, the nodes it affects and the evidence.',
  ].join('\n\n')
}

// The re-plan turn (SP-ARCH-2): the next turn of the architect's thread, as a DESIGN delta for the nodes that have not started. The
// packet names the reasons and the numbers behind them, the head the turn judges and the range from the head the DESIGN ran at.
export function replanPacket(ms, cur, due, head, misaligned) {
  const at = headBlock({ head, base_tip: cur.base_tip, landed: cur.landed })
  const lines = due.reasons.map((r) => r)
  const parts = [
    'ROLE: architect. Re-plan milestone ' + ms.id + ' (' + ms.title + '), on the thread of your DESIGN (revision ' + cur.revision + '). Your directory P is a read-only checkout of the head below: work only under the absolute path P.',
    at.head,
    section('WHY THE MILESTONE RE-PLANS', lines),
    'PLAN MISMATCHES OF THIS REVISION: ' + due.plan_mismatch + ' (calls ' + (due.plan_mismatch_calls.join(', ') || 'none') + ')',
    'SLICES OF THE PLAN CHANGED SINCE THIS REVISION: ' + due.slice_change.changed.length + ' of ' + due.slice_change.total + (due.slice_change.changed.length ? ' (' + due.slice_change.changed.join(', ') + ')' : ''),
    section('DECISIONS OF THE DESIGN', drivenDecisions(cur)),
    at.landed,
  ]
  if (misaligned && misaligned.length) {
    parts.push(section('DECISIONS THE REFLECTION FOUND THE RANGE TO DEPART FROM', misaligned.map((m) => '- ' + m.decision_id + ': ' + m.observed + ' (nodes ' + (m.affected_nodes || []).join(', ') + ')')))
  }
  parts.push('Answer with DESIGN: the delta of the plan. Keep the ids of the decisions you do not change. Lanes that have started keep the revision they were released under; the issues of nodes that have not started take this one, and the correction issues the orchestrator adds are planned in it.')
  return parts.join('\n\n')
}

// ends are the end condition commands of the milestone's issues the explorer runs once (CRW-1382): given to the first explorer of a
// DESIGN's first audit round only, which answers FINDINGS_END; every other packet is the one it was before CRW-1382, word for word.
export function explorerPacket(kind, route, ctx, ends) {
  const parts = [
    'ROLE: explorer. Answer one question by reading the repository; do not edit anything.',
    'ASKED BY: ' + (ctx.node ? 'node ' + ctx.node : 'the architect of milestone ' + ctx.milestone) + ' (' + kind + ')',
    'QUESTION: ' + route.question,
    section('FILES NEEDED', list(route.files_needed, 'not named')),
  ]
  if (ends && ends.length) {
    parts.push(section('END CONDITIONS (run each command once where you are, change nothing, and report each in end_conditions with its exit code, the number of tests it ran and a one-line summary; a failure or a run of no test is reported, never fixed)',
      ends.map((e) => '- ' + e.issue + ': `' + e.command + '`')))
  }
  parts.push('Answer with FINDINGS: the answer, the evidence, and what you verified, inferred and could not determine' + (ends && ends.length ? ', and end_conditions.' : '.'))
  return parts.join('\n\n')
}

export function packet(node, plan, checkpoint, notes) {
  const parts = [
    'ROLE: executor. Node ' + node + ' (' + plan.issue + ': ' + plan.title + ').',
    'DESIGN REVISION: ' + plan.revision + ' ' + plan.design_digest,
    section('APPROACH', [plan.approach]),
    section('EDIT REGIONS', list(plan.regions, 'undeclared')),
    section('TESTS FIRST', list(plan.tests_first)),
    section('DONE CONDITION', [plan.done_condition]),
    section('CRITERIA', criteriaList(plan.criteria)),
    section('DECISIONS', plan.decisions.map((d) => '- ' + d.id + ': ' + d.change)),
  ]
  if (plan.answers && plan.answers.length) parts.push(section('ANSWERS TO THE OPEN QUESTIONS OF THE DESIGN', plan.answers.map((a) => '- ' + a.question + (a.answer ? '\n  ' + a.answer : ''))))
  const n = notesSection(notes)
  if (n) parts.push(n)
  if (checkpoint) {
    parts.push(section('CHECKPOINT (a fresh tier takes over; the worktree starts at this head)', [
      '- head: ' + checkpoint.head, '- tree: ' + checkpoint.tree,
    ]))
  }
  parts.push('Answer with RECEIPT when you are done. Edit files only under the absolute path P of your worktree.')
  return parts.join('\n\n')
}

export function answerPacket(answers) {
  return [
    'You asked for research. The answers follow; continue the same task.',
    section('ANSWERS', answers.map((a) => '- ' + a.question + '\n  ' + (a.answer || (a.route === 'inline' ? '(answer it yourself: the files are inside your regions or few)' : '(no answer)')))),
  ].join('\n\n')
}

export function fixPacket(node, verdict, ci, notes, receipt) {
  const parts = ['ROLE: executor. Node ' + node + ': fix what the run found, in the same worktree, and answer with RECEIPT again.']
  if (ci && !ci.pass) parts.push(section('VERIFICATION FAILED (' + ci.command + ')', list(ci.failures.length ? ci.failures : [ci.summary])))
  if (verdict) {
    parts.push(section('REVIEW (' + verdict.verdict + ')', verdict.defects.map((d) => '- [' + d.severity + '/' + d.kind + '] ' + d.ref + ' ' + d.file + ':' + d.line + ' ' + d.finding)))
    parts.push(section('CRITERIA NOT MET', list(verdict.criteria.filter((c) => !c.met).map((c) => c.id + ': ' + c.evidence), 'none')))
  }
  const n = notesSection(notes)
  if (n) parts.push(n)
  if (receipt && receipt.summary) parts.push('YOUR LAST RECEIPT: ' + receipt.summary)
  return parts.join('\n\n')
}

export function reviewPacket(node, plan, head, ci, opts) {
  const parts = [
    'ROLE: reviewer. Review the head ' + head.commit + ' of node ' + node + ' (' + plan.issue + ') against its criteria; round ' + (opts.round || 1) + '.',
    // the two lines `crw flow audit` reads back from the thread on the host: what the acceptance of this review stands on
    'HEAD: ' + head.commit + '\nCRITERIA DIGEST: ' + plan.criteria_digest,
    section('CRITERIA', criteriaList(plan.criteria)),
    section('DONE CONDITION', [plan.done_condition]),
    section('EDIT REGIONS', list(plan.regions, 'undeclared')),
    section('CHANGED PATHS', list(head.changed)),
    section('OUT OF SCOPE', list(head.out_of_scope, 'none')),
    'VERIFICATION: ' + (ci.pass ? 'passed' : 'failed') + ' (' + ci.command + ') record ' + ci.digest,
  ]
  const n = notesSection(opts.notes)
  if (n) parts.push(n)
  parts.push('Answer with VERDICT. verified means every criterion is met and no defect blocks. Answer one entry of criteria for each criterion c1, c2, ... above, each once, under its id. A defect you cannot judge from the head is of kind evidence_missing and carries an evidence_question. The impact of each defect is one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable: a defect of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block. A verdict of verified over a defect that blocks is refused as no verdict.')
  return parts.join('\n\n')
}

// ---- the milestone close (crw:lib/close) ----

// The captures as the later calls read them: the path each can be opened at (crw.qaCapture resolved it against the directory the
// capture call ran in), not the path relative to that call's own worktree.
function captureLines(captures) {
  return list((captures || []).map((c) => c.kind + ' ' + (c.abs || c.path) + ': ' + c.shows))
}

function decisionLines(decisions) {
  return list((decisions || []).map((d) => d.id + ' (' + ((d.nodes || []).length ? 'nodes ' + d.nodes.join(', ') : 'no node') + '): ' + d.change), 'none recorded')
}

function evidenceLines(g) {
  return list((g.evidence || []).map((e) => e.kind + (e.command ? ' ' + e.command : '') + ': ' + e.state + (e.pass ? ' pass' : '') + (e.head ? ' at ' + e.head : '')))
}

// The QA capture call (SP-QA-1): drives the surface in a scratch worktree at the head the gate pinned and judges nothing.
export function qaCapturePacket(ms, g) {
  return [
    'ROLE: executor. QA capture for the final gate of milestone ' + ms.id + ' (' + ms.title + ') at the integrated head ' + g.head + '.',
    'HEAD: ' + g.head,
    section('SURFACES', list(g.surfaces)),
    section('CRITERIA OF THE MILESTONE', list(ms.criteria)),
    'Follow sections 1 to 4 of the crw-qa skill: state the surface and the invocation, drive it, capture the artifacts of every scenario, probe the adversarial classes, and tear down what you started. Your directory P is a scratch copy of the head: work only under the absolute path P, and whatever you edit there is discarded. Do not start helpers: the run starts the reviewer passes itself.',
    'Answer with QA_CAPTURE: every capture you made (path, kind, one line on what it shows), the scenarios you drove, and the teardown lines. A capture that is not listed does not exist for the gate. Do not judge the captures in this answer.',
  ].join('\n\n')
}

const PASS_FOCUS = {
  A: 'Pass A - design and functional integrity: is this a real, coherent implementation built on the reused primitives of the project (not a mock-only screen or a pasted raster), and do the intended features work?',
  B: 'Pass B - visual fidelity and CJK precision: open the captures directly; hunt clipping, baseline drop, glyph breakage, KO/JA/ZH precision, and border or box-drawing drift.',
}

// One of the two oracle passes (SP-QA-2): read-only, over the same capture packet.
export function qaPassPacket(pass, ms, g, view) {
  return [
    'ROLE: explorer. QA pass ' + pass + ' for the final gate of milestone ' + ms.id + ' at the integrated head ' + g.head + '. Read only; edit nothing.',
    'HEAD: ' + g.head,
    PASS_FOCUS[pass],
    section('CAPTURES (the same packet goes to both passes; open the paths given here directly)', captureLines(view.captures)),
    section('SCENARIOS DRIVEN', list(view.matrix)),
    section('TEARDOWN', list(view.teardown)),
    section('CRITERIA OF THE MILESTONE', list(ms.criteria)),
    'Answer with QA_PASS: pass "' + pass + '", a verdict of PASS, REVISE or FAIL, the findings with their severity, and a summary. A PASS stands over no P0 or P1 finding.',
  ].join('\n\n')
}

// The next turn of the QA call's thread (C3 and above): synthesise the two passes into the receipt.
export function qaSynthesisPacket(ms, g, passes) {
  return [
    'The two reviewer passes of the final gate of milestone ' + ms.id + ' at ' + g.head + ' are back. Synthesise them into one QA receipt.',
    section('PASS RESULTS', passes.map((p) => '- pass ' + p.pass + ': ' + p.verdict + ' - ' + p.summary + (p.findings.length ? '\n  ' + p.findings.map((f) => '[' + f.severity + '] ' + f.ref + ': ' + f.finding).join('\n  ') : ''))),
    'Answer with QA_RECEIPT: PASS only when both passes found nothing that stands against the criteria; REVISE when something must change; FAIL when the surface does not work.',
  ].join('\n\n')
}

// The next turn of the QA call's thread (C2): one self-review of its own captures is the receipt.
export function qaSelfReviewPacket(ms, g, view) {
  return [
    'Review your own captures of the final gate of milestone ' + ms.id + ' at ' + g.head + ' once, as a reviewer who did not make them, and give the QA receipt.',
    section('YOUR CAPTURES (open the paths given here directly: they are resolved against the worktree you captured in)', captureLines(view.captures)),
    'Answer with QA_RECEIPT: PASS, REVISE or FAIL, the findings with their severity, and a summary. A PASS stands over no P0 or P1 finding.',
  ].join('\n\n')
}

// reviewer-final: the review of the integrated range, after the final gate passed. The gate is its input.
export function finalPacket(ms, g) {
  return [
    'ROLE: reviewer-final. Review the integrated range ' + g.range + ' of milestone ' + ms.id + ' (' + ms.title + ') after its final gate passed. You review the range, not the issues one by one: those were reviewed before.',
    'HEAD: ' + g.head + '\nRANGE: ' + g.range,
    section('FINAL GATE (passed)', evidenceLines(g)),
    section('CRITERIA OF THE MILESTONE', criteriaList(ms.criteria)),
    // the approved DESIGN lives in the journal's ledger, not in the checkout this reviewer reads: its decisions are the comparison
    // source of the alignment judgement
    section('DESIGN DECISIONS (the approved DESIGN of this milestone; name a decision by its id in the drift)', decisionLines(g.decisions)),
    'Answer with VERDICT_FINAL: the verdict, one entry of criteria for each criterion c1, c2, ... above, the defects, and the alignment of the integrated range with the DESIGN DECISIONS above (ALIGNED, MISALIGNED with the drift you saw, or NOT_ASSESSED). verified means every criterion is met and no defect of P0 to P2 stands.',
  ].join('\n\n')
}
