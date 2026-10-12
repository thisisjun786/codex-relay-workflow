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
