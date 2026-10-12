// crw:lib/packets: the packets the project lane sends - plain text, built from the journal's facts and nothing else, so the same
// facts give the same bytes (the digest of a packet is part of the input digest of its call, and a resumed lane must build the
// packet it built). Every function is pure.

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

export function designAuditPacket(ms, cand, round) {
  return [
    'ROLE: reviewer. Audit the DESIGN of milestone ' + ms.id + ' (round ' + round + ') before any executor starts. You did not write it and are of another model family than the architect.',
    'Answer with DESIGN_AUDIT: approved only when every issue node has a plan an executor can carry out inside its edit regions, the decisions are consistent with the plan, and no P0 or P1 finding stands; otherwise needs_changes with the findings the architect must act on.',
    section('DESIGN', [canon(cand.design)]),
    section('ANSWERS TO THE OPEN QUESTIONS', [canon(cand.answers)]),
  ].join('\n\n')
}

export function designFixPacket(audit) {
  return [
    'ROLE: architect. The independent audit of your DESIGN found what follows. Answer with the DESIGN again, with every finding addressed.',
    section('FINDINGS', audit.findings.map((f) => '- [' + f.severity + '] ' + f.ref + ': ' + f.finding)),
    audit.summary ? 'AUDIT SUMMARY: ' + audit.summary : '',
  ].filter(Boolean).join('\n\n')
}

function landedLines(landed) {
  return list((landed || []).map((l) => l.node + ': ' + l.head), 'none landed')
}

function drivenDecisions(cur) {
  return list(((cur.design && cur.design.decisions) || []).map((d) => d.id + ': ' + d.change), 'none recorded')
}

// The REFLECTION turn (SP-ARCH-3): the next turn of the architect's thread, at the integrated head the final gate pinned. The packet
// names the head, the range from the head the DESIGN ran at, and the head every node landed with; the turn's directory is a detached
// read-only worktree of that head.
export function reflectionPacket(ms, cur, closed) {
  const rng = (cur.base_tip ? cur.base_tip + '..' : '') + closed.gate.head
  return [
    'ROLE: architect. Reflection at the close of milestone ' + ms.id + ' (' + ms.title + '), on the thread of your DESIGN (revision ' + cur.revision + '). Your working directory is a read-only checkout of the integrated head.',
    'HEAD: ' + closed.gate.head + '\nDESIGN RAN AT: ' + (cur.base_tip || 'unknown') + '\nRANGE: ' + rng,
    section('DECISIONS OF THE DESIGN (map each to the files and criteria of the integrated range)', drivenDecisions(cur)),
    section('LANDED HEADS', landedLines(cur.landed)),
    'REVIEWER-FINAL ALIGNMENT: ' + (closed.final ? closed.final.alignment : 'none'),
    'Answer with REFLECTION: ALIGNED, or MISALIGNED with each decision the range departs from, what you observed, the nodes it affects and the evidence.',
  ].join('\n\n')
}

// The re-plan turn (SP-ARCH-2): the next turn of the architect's thread, as a DESIGN delta for the nodes that have not started. The
// packet names the reasons and the numbers behind them, the head the turn judges and the range from the head the DESIGN ran at.
export function replanPacket(ms, cur, due, head, misaligned) {
  const rng = (cur.base_tip ? cur.base_tip + '..' : '') + head
  const lines = due.reasons.map((r) => r)
  const parts = [
    'ROLE: architect. Re-plan milestone ' + ms.id + ' (' + ms.title + '), on the thread of your DESIGN (revision ' + cur.revision + '). Your working directory is a read-only checkout of the head below.',
    'HEAD: ' + head + '\nDESIGN RAN AT: ' + (cur.base_tip || 'unknown') + '\nRANGE: ' + rng,
    section('WHY THE MILESTONE RE-PLANS', lines),
    'PLAN MISMATCHES OF THIS REVISION: ' + due.plan_mismatch + ' (calls ' + (due.plan_mismatch_calls.join(', ') || 'none') + ')',
    'SLICES OF THE PLAN CHANGED SINCE THIS REVISION: ' + due.slice_change.changed.length + ' of ' + due.slice_change.total + (due.slice_change.changed.length ? ' (' + due.slice_change.changed.join(', ') + ')' : ''),
    section('DECISIONS OF THE DESIGN', drivenDecisions(cur)),
    section('LANDED HEADS', landedLines(cur.landed)),
  ]
  if (misaligned && misaligned.length) {
    parts.push(section('DECISIONS THE REFLECTION FOUND THE RANGE TO DEPART FROM', misaligned.map((m) => '- ' + m.decision_id + ': ' + m.observed + ' (nodes ' + (m.affected_nodes || []).join(', ') + ')')))
  }
  parts.push('Answer with DESIGN: the delta of the plan. Keep the ids of the decisions you do not change. Lanes that have started keep the revision they were released under; the issues of nodes that have not started take this one, and the correction issues the orchestrator adds are planned in it.')
  return parts.join('\n\n')
}

export function explorerPacket(kind, route, ctx) {
  return [
    'ROLE: explorer. Answer one question by reading the repository; do not edit anything.',
    'ASKED BY: ' + (ctx.node ? 'node ' + ctx.node : 'the architect of milestone ' + ctx.milestone) + ' (' + kind + ')',
    'QUESTION: ' + route.question,
    section('FILES NEEDED', list(route.files_needed, 'not named')),
    'Answer with FINDINGS: the answer, the evidence, and what you verified, inferred and could not determine.',
  ].join('\n\n')
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
  parts.push('Answer with RECEIPT when you are done. Edit files only inside your worktree.')
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
  parts.push('Answer with VERDICT. verified means every criterion is met and no defect of P0 to P2 stands. Answer one entry of criteria for each criterion c1, c2, ... above, each once, under its id. A defect you cannot judge from the head is of kind evidence_missing and carries an evidence_question.')
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
    'Follow sections 1 to 4 of the crw-qa skill: state the surface and the invocation, drive it, capture the artifacts of every scenario, probe the adversarial classes, and tear down what you started. Your worktree is a scratch copy of the head; whatever you edit there is discarded. Do not start helpers: the run starts the reviewer passes itself.',
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
