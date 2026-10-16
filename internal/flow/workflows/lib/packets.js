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
// past its design audit comes to the same refusal of crw.designLedger. CRW-1469 gives the packet of a fresh executor thread the
// node's last state (packet's last: LAST FINDINGS, LAST VERIFICATION, LAST RECEIPT): the first build of a node has none and its packet
// is the one it was, but a rebuild (after a refusal or a null_model) or the executor-t1 build of a node that had a unit-check finding,
// a verification or a receipt before it has another packet than an earlier binary built, so a resume over a journal recorded under
// the earlier text runs that call live again, and the rest of the node's path after it; the fix packet's text is kept. The
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

// architectPacket is the architect's first turn of a milestone. background is the answers of the background explorers that ran before
// it, one for each node (crw.explore of kind background, CRW-1466): given, the packet carries them as BACKGROUND FINDINGS, each by its
// id, question, answer and the explorer's findings only - never by its route, reason, scope, notation or origin, so the packet's bytes
// do not move with the routing metadata a binary records; not given (or empty), the packet is the one it was before CRW-1466. An
// answer whose explorer was asked twice and did not answer (missing, CRW-1571) is not a finding: a MISSING EXPLORATIONS section after
// them names it, and with none missing the packet is the one it was before.
// head is the design head the run sampled before the phase explore (crw.designHead): the background explorers read it and the
// architect's directory is a checkout of it, and BACKGROUND FINDINGS names it; without one the packet is as it was.
export function architectPacket(ms, background, head) {
  const nodes = ms.nodes.map((n) => n.id + ' (' + n.issue + '): ' + n.title + (n.after.length ? ' after ' + n.after.join(', ') : '') + (n.group ? ' [group ' + n.group + ']' : '') +
    '\n  regions: ' + (n.regions.length ? n.regions.join(', ') : 'undeclared') + '\n  criteria: ' + (n.criteria.length ? n.criteria.join(' | ') : 'none'))
  const parts = [
    'ROLE: architect. Plan milestone ' + ms.id + ' of plan ' + ms.plan + ' (' + ms.title + ').',
    'Answer with DESIGN: the decisions, one plan for each issue node below (its size_fit, the sub_tasks of a split node, edit regions, approach, tests first and done condition), the open questions you cannot answer from what you read, and the risks.',
    section('NODES', nodes),
  ]
  const answered = (background || []).filter((a) => !a.missing)
  const missing = (background || []).filter((a) => a.missing)
  if (answered.length) {
    parts.push(section('BACKGROUND FINDINGS (' + (head ? 'read at ' + head + ', the commit your directory is a checkout of; ' : '') + 'an explorer read the repository for each node before you; build on them, and ask as an open question only what they and your own reading leave open)',
      answered.map((a) => {
        const lines = [(a.id ? a.id + ': ' : '') + a.question]
        if (a.answer) lines.push('  answer: ' + a.answer)
        if (a.findings) lines.push('  findings: ' + canon(a.findings))
        return '- ' + lines.join('\n')
      })))
  }
  // a background question whose explorer was null_model twice (crw.explore's missing, CRW-1571): named, never listed as a finding
  if (missing.length) {
    parts.push(section('MISSING EXPLORATIONS (the explorer of each was asked twice and did not answer; read what the node rests on yourself, and ask as an open question what you cannot settle)',
      missing.map((a) => '- ' + missingLine(a))))
  }
  return parts.join('\n\n')
}

// missingLine names a question whose explorer was asked twice and did not answer (crw.explore's missing, CRW-1571): its id and
// question, both calls, and the status and reason of the last.
function missingLine(a) {
  const m = a.missing
  return (a.id ? a.id + ': ' : '') + a.question + ' (calls ' + m.calls.join(', ') + ': ' + m.status + (m.reason ? ' ' + m.reason : '') + ')'
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
// (CRW-1347), so no question reaches the audit as an empty answer without its route. A question whose explorer was asked twice and
// did not answer (crw.explore's missing, CRW-1571; a repeat of it in the round carries it too) says so in its route line, so the
// critic never reads it as answered; with none missing the lines are the ones they were before.
function answerLines(answers) {
  return list((answers || []).map((a) => {
    const lines = [(a.id ? a.id + ': ' : '') + a.question, '  route: ' + routeLine(a)]
    if (a.answer) lines.push('  answer: ' + a.answer)
    if (a.findings) lines.push('  findings: ' + canon(a.findings))
    return lines.join('\n')
  }), 'no open question')
}

function routeLine(a) {
  if (a.missing) return 'an explorer was asked twice and did not answer (' + missedCalls(a.missing) + ')'
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

// missedCalls names both calls of a missing exploration and the status and reason of the last (CRW-1571).
function missedCalls(m) {
  return 'calls ' + m.calls.join(', ') + ': ' + m.status + (m.reason ? ' ' + m.reason : '')
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
// context is a re-plan's {head, base_tip, landed, unstarted, released} (CRW-1458): the critic's directory is then a read-only checkout of the
// head the re-plan judges, and the packet names it as the re-plan's architect turn has it (headBlock); the first DESIGN has none.
// unstarted is the ids of the nodes the re-plan releases (CRW-1462; none at the close): the re-plan answers with the whole DESIGN in
// every round, its design-fix answers included, since an approved one becomes the next revision on its own, so a node with no issue
// in it is a P1 finding. An open question of the DESIGN with no entry in the answers is a P1 finding too (CRW-1466): the DESIGN the
// critic reads is the architect's answer to the round's answers, and a question raised there has not been routed. released is the
// plans the started nodes were released with (crw.releasedPlans): the critic compares the DESIGN with them (RELEASED PLANS).
export function designAuditPacket(ms, cand, round, prior, context) {
  const at = context ? headBlock(context) : null
  const replanned = context && Array.isArray(context.unstarted)
  return [
    'ROLE: critic. Audit the DESIGN of milestone ' + ms.id + ' (round ' + round + ') before any executor starts. You did not write it and are of another model family than the architect.',
    'Answer with DESIGN_AUDIT: approved only when every issue node has a plan an executor can carry out inside its edit regions, every done condition can be met under the executor contract below, the decisions are consistent with the plan, no decision of the DESIGN is outside the architect\'s authority below, and no P0 or P1 finding stands; otherwise needs_changes with the findings the architect must act on. Check every done condition against the executor contract and every decision against the authority boundary: a done condition the contract cannot meet and a decision outside the authority are each a P1 finding that names its node or decision.',
    section('EXECUTOR CONTRACT (every done condition must be met under it)', EXECUTOR_CONTRACT),
    section('OUT-OF-AUTHORITY DECISIONS (the architect raises these as open questions and does not decide them)', AUTHORITY),
    round > 1 && prior ? section('PRIOR FINDINGS (the findings of the earlier rounds by id, each with the architect\'s disposition: folded, rebutted, or not reported)', priorLines(prior)) +
      '\nJudge each prior finding: a folded one is resolved only if this DESIGN does fold it; a rebuttal you do not accept is raised again in your findings under the same ref, with the reason you reject it; a finding not reported, or of a round whose dispositions are null, is still open.' : '',
    at ? 'Your directory is a read-only checkout of the pinned head: the DESIGN re-plans the milestone at it.\n' + at.head : '',
    at ? at.landed : '',
    replanned ? unstartedSection(ms, context.unstarted) : '',
    replanned ? 'The DESIGN is the whole DESIGN of the milestone, not a delta, in every round of this audit, the architect\'s design-fix answers included: every node of the milestone has its issue in it, the started ones as they were released; an unstarted node with no issue in the DESIGN is a P1 finding that names the node, and so is a started node whose issue is missing.' : '',
    context && Array.isArray(context.released) ? releasedSection(context.released) +
      '\nCompare the DESIGN with it: a started node whose issue in the DESIGN differs from its plan in RELEASED PLANS in any field is a P1 finding that names the node, and so is a decision a released plan names that the DESIGN drops or changes in any field.' : '',
    section('DESIGN', [canon(cand.design)]),
    section('ANSWERS TO THE OPEN QUESTIONS (each with its route and what it got)', answerLines(cand.answers)),
    'An open question of the DESIGN with no entry in ANSWERS TO THE OPEN QUESTIONS (one the architect raised when the answers came back to its thread, which no route has taken yet) is a P1 finding that names the question, so the next round routes it.',
  ].filter(Boolean).join('\n\n')
}

// releasedSection lists the plans the started nodes were released with (crw.releasedPlans): each node with the revision it was
// released under, its whole issue plan (every field) and the whole decisions its decision_refs name, so the critic sees any drift.
// An entry with no plan member (the flat form an earlier lane binary recorded) is rendered from its own fields. A node sized for a
// split without sub-tasks (the legacy row of a DESIGN recorded before S-1), a child or a bundle split (kind) is listed with that: it
// started, its lane escalated it, and no executor started. A bundle split says a bundle_split_request escalation was raised for it and
// names its id (escalation_id, CRW-1437); before that escalation is recorded it says only that its lane escalates it so. It makes no
// claim about whether the request reached the 부모: crw.releasedPlans is replayed on a resume, and a delivery can change after it was
// read. A split with sub-tasks is released to an executor (CRW-1459) and is listed as released, as an executor node is.
function releasedSection(released) {
  // how a started node started: released, or sized for a node its lane escalated as size_split with no executor
  const how = (p) => {
    if (p.kind === 'split') return 'sized for a split under ' + p.revision + ' with no sub-tasks (a legacy split row: no executor started; its lane escalated it as size_split)'
    if (p.kind === 'child') return 'sized for a child under ' + p.revision + ' (no executor started; its lane escalated it as size_split)'
    if (p.kind === 'bundle_split') {
      const raised = p.escalation_id ? 'a bundle_split_request escalation was raised for it: ' + p.escalation_id
        : 'its lane escalates it as size_split with ask bundle_split_request'
      return 'sized for a bundle split under ' + p.revision + ' (no executor started; ' + raised + ')'
    }
    return 'released under ' + p.revision
  }
  return section('RELEASED PLANS (the started nodes, each with the whole issue plan of the revision it was released under and the decisions it names)', released.length === 0 ? ['- none'] : released.map((p) => {
    const plan = p.plan || p
    return '- ' + p.node + ' (' + plan.issue + '), ' + how(p) + '\n  plan: ' + canon(plan) + '\n  decisions: ' + canon(p.decisions || [])
  }))
}

// answeredQuestions are the answers of a round's open questions that hold an answer for the architect (CRW-1466): a question an
// explorer answered (route spawn, with its findings) or that reused an answer the run has (route reuse, with that answer). A question
// asked of the orchestrator, or one no route answered, is not among them: the critic reads it in its ANSWERS TO THE OPEN QUESTIONS.
export function answeredQuestions(answers) {
  return (answers || []).filter((a) => (a.route === 'spawn' && a.findings) || (a.route === 'reuse' && a.answer))
}

// The design-answers turn of the architect's thread (CRW-1466): the answers its open questions got in this audit round go back to it
// before the critic audits the DESIGN, and it answers with the DESIGN again. Each answered question is rendered by its id, question,
// answer and the explorer's findings only - never by its route, reason, scope, notation or origin, so the packet's bytes do not move
// with the routing metadata a binary records. A question whose explorer was asked twice and did not answer (crw.explore's missing,
// CRW-1571) is no answer: a NOT ANSWERED section after the ANSWERS names it, and with none missing the packet is the one it was
// before. context is a re-plan's {head, base_tip, landed, unstarted}: the turn is then at the head the re-plan judges (headBlock); the
// first DESIGN has none.
export function designAnswersPacket(answers, context) {
  const at = context ? headBlock(context) : null
  const missing = (answers || []).filter((a) => a.missing)
  return [
    'ROLE: architect. The open questions of your DESIGN got the answers below. Answer with the DESIGN again, with what they say folded into its decisions and plans, before the critic audits it; keep the ids of the decisions and the issues, and give dispositions null (no audit finding is before you). An open question an answer settles leaves open_questions; a question you raise anew in this answer reaches the critic with no answer, which is a P1 finding, and is routed in the next audit round.',
    section('ANSWERS', answeredQuestions(answers).map((a) => {
      const lines = [(a.id ? a.id + ': ' : '') + a.question]
      if (a.answer) lines.push('  answer: ' + a.answer)
      if (a.findings) lines.push('  findings: ' + canon(a.findings))
      return '- ' + lines.join('\n')
    })),
    missing.length ? section('NOT ANSWERED (the explorer of each was asked twice and did not answer; the question stays open)', missing.map((a) => '- ' + missingLine(a))) : '',
    at ? 'Your directory P is a read-only checkout of the pinned head: work only under the absolute path P.\n' + at.head : '',
    at ? at.landed : '',
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
  const parts = [
    'ROLE: architect. Reflection at the close of milestone ' + ms.id + ' (' + ms.title + '), ' + (cur.handle ? 'on the thread of your DESIGN (revision ' + cur.revision + ')' : 'using the approved DESIGN reused from run ' + cur.source_run + ' revision ' + cur.source_revision) + '. Your directory P is a read-only checkout of the integrated head: work only under the absolute path P.',
  ]
  if (cur.source_run && !cur.handle) {
    parts.push('REUSED APPROVED DESIGN:\n' + JSON.stringify(cur.design) + '\nAPPROVED ANSWERS:\n' + JSON.stringify(cur.answers || []))
  }
  parts.push(
    at.head,
    section('DECISIONS OF THE DESIGN (map each to the files and criteria of the integrated range)', drivenDecisions(cur)),
    at.landed,
    'Answer with REFLECTION: ALIGNED, or MISALIGNED with each decision the range departs from, what you observed, the nodes it affects and the evidence.',
  )
  return parts.join('\n\n')
}

// unstartedSection names the nodes a re-plan releases under its revision (CRW-1462): each by id, issue and title; none at the close.
function unstartedSection(ms, ids) {
  return section('UNSTARTED NODES (released under the revision this re-plan makes)', list((ids || []).map((id) => {
    const n = ms.nodes.find((x) => x.id === id)
    return n ? id + ' (' + n.issue + '): ' + n.title : id
  })))
}

// The re-plan turn (SP-ARCH-2): the next turn of the architect's thread, answered with the whole DESIGN of the milestone (CRW-1462):
// every decision and every issue of every node, the started ones as they were released, with the ids kept, so the revision it makes
// stands on its own. The packet names the reasons and the numbers behind them, the head the turn judges, the range from the head the
// DESIGN ran at, and the nodes that have not started (unstarted, their ids; none at the close).
export function replanPacket(ms, cur, due, head, misaligned, unstarted) {
  const at = headBlock({ head, base_tip: cur.base_tip, landed: cur.landed })
  const lines = due.reasons.map((r) => r)
  const parts = [
    'ROLE: architect. Re-plan milestone ' + ms.id + ' (' + ms.title + '), ' + (cur.handle ? 'on the thread of your DESIGN (revision ' + cur.revision + ')' : 'using the approved DESIGN reused from run ' + cur.source_run + ' revision ' + cur.source_revision) + '. Your directory P is a read-only checkout of the head below: work only under the absolute path P.',
  ]
  if (cur.source_run && !cur.handle) {
    parts.push('REUSED APPROVED DESIGN:\n' + JSON.stringify(cur.design) + '\nAPPROVED ANSWERS:\n' + JSON.stringify(cur.answers || []))
  }
  parts.push(
    at.head,
    section('WHY THE MILESTONE RE-PLANS', lines),
    'PLAN MISMATCHES OF THIS REVISION: ' + due.plan_mismatch + ' (calls ' + (due.plan_mismatch_calls.join(', ') || 'none') + ')',
    'SLICES OF THE PLAN CHANGED SINCE THIS REVISION: ' + due.slice_change.changed.length + ' of ' + due.slice_change.total + (due.slice_change.changed.length ? ' (' + due.slice_change.changed.join(', ') + ')' : ''),
    section('DECISIONS OF THE DESIGN', drivenDecisions(cur)),
    at.landed,
    unstartedSection(ms, unstarted),
  )
  if (misaligned && misaligned.length) {
    parts.push(section('DECISIONS THE REFLECTION FOUND THE RANGE TO DEPART FROM', misaligned.map((m) => '- ' + m.decision_id + ': ' + m.observed + ' (nodes ' + (m.affected_nodes || []).join(', ') + ')')))
  }
  parts.push('Answer with DESIGN: the whole DESIGN of the milestone, as the next revision of it: every decision and every issue of every node, the nodes that have started with their issues as they were released and the unstarted nodes above with their issues for this revision. Keep the ids of the decisions and of the issues. Lanes that have started keep the revision they were released under; the issues of the unstarted nodes take this one, and the correction issues the orchestrator adds are planned in it.')
  return parts.join('\n\n')
}

// ends are the end condition commands of the milestone's issues the explorer runs once (CRW-1382): given to the first explorer of a
// DESIGN's first audit round only, which answers FINDINGS_END; every other packet is the one it was before CRW-1382, word for word.
export function explorerPacket(kind, route, ctx, ends) {
  const parts = [
    'ROLE: explorer. Answer one question by reading the repository; do not edit anything.',
    // a background question is the run's, asked for the architect before its first DESIGN (CRW-1466); every other kind is as before
    'ASKED BY: ' + (kind === 'background' ? 'the run, in the background of the architect of milestone ' + ctx.milestone : ctx.node ? 'node ' + ctx.node : 'the architect of milestone ' + ctx.milestone) + ' (' + kind + ')',
    'QUESTION: ' + route.question,
    section('FILES NEEDED', list(route.files_needed, 'not named')),
  ]
  // a background explorer reads the design head the run sampled for the milestone's first DESIGN, as the architect does
  if (kind === 'background' && ctx.head) parts.push('READ AT: ' + ctx.head + ' (your directory is a read-only checkout of this commit, the one the architect plans on)')
  if (ends && ends.length) {
    parts.push(section('END CONDITIONS (run each command once where you are, change nothing, and report each in end_conditions with its exit code, the number of tests it ran and a one-line summary; a failure or a run of no test is reported, never fixed)',
      ends.map((e) => '- ' + e.issue + ': `' + e.command + '`')))
  }
  parts.push('Answer with FINDINGS: the answer, the evidence, and what you verified, inferred and could not determine' + (ends && ends.length ? ', and end_conditions.' : '.'))
  return parts.join('\n\n')
}

// findingLines are the findings of a unit check, each by its severity, id, file, line and text: the executor's packet and the fix
// packet name them alike.
function findingLines(findings) {
  return list(findings.map((f) => '[' + f.severity + '] ' + f.id + ' ' + f.file + ':' + f.line + ' ' + f.text))
}

// findingsSection renders the findings of a unit check under title; no findings, no section.
function findingsSection(title, findings) {
  if (!findings || findings.length === 0) return ''
  return section(title, findingLines(findings))
}

// ciSection renders a verification under the prefix: passed with its command, or failed with its command and its failures (its
// summary when it names none), then the failed tests its record names (failed_tests, CRW-1469: by failed step, its tests, its
// packages and the last lines of its output); no verification, no section. A failed verification with no failed tests, as every one
// recorded before them, renders the text it did.
function ciSection(prefix, ci) {
  if (!ci) return ''
  if (ci.pass) return prefix + ' PASSED (' + ci.command + ')'
  const failed = section(prefix + ' FAILED (' + ci.command + ')', list(ci.failures && ci.failures.length ? ci.failures : [ci.summary]))
  if (!Array.isArray(ci.failed_tests) || ci.failed_tests.length === 0) return failed
  return failed + '\n\n' + section('FAILED TESTS (each failed step of the verification record: its tests, its packages and the last lines of its output)',
    ci.failed_tests.map(failedStepLine))
}

// failedStepLine is one failed step of a verification: its job and step, the tests it names (or that it names none) with their
// packages, and the last lines of its output indented under it.
function failedStepLine(s) {
  const tests = s.tests && s.tests.length ? s.tests.join(', ') : 'no test named'
  const packages = s.packages && s.packages.length ? ' (packages ' + s.packages.join(', ') + ')' : ''
  const tail = s.tail ? '\n' + s.tail.split('\n').map((l) => '    ' + l).join('\n') : ''
  return '- ' + s.job + '/' + s.step + ': ' + tests + packages + tail
}

// receiptSection renders the summary of a receipt under title; a receipt with no summary, no section.
function receiptSection(title, receipt) {
  if (!receipt || !receipt.summary) return ''
  return title + ': ' + receipt.summary
}

// packet is the first turn of a fresh executor thread of a node: the first build, a rebuild after a refusal or a null_model, the
// executor-t1 build. last is the node's last state, which the node lane keeps (CRW-1469): {findings, ci, receipt}, the findings of the
// last unit check (none after a check that found nothing), the last verification and the last receipt of the node. A fresh thread
// has none of the turns of the thread before it, so the packet carries each that is there; with none (the first build) the packet is
// the one it was before CRW-1469. The old reviewer path, which an earlier run replays, passes its explorer notes (an array) in that
// place: the packet carries them as FINDINGS OF THE EXPLORER, the text it was before CRW-1469.
// A node sized for a split names its sub-tasks (CRW-1459): last.split is the lane's progress over them, {done, failed} (the sub-tasks
// with a checkpoint commit, from the first, and the receipts of those that reported failed, by id), and the packet carries SUB-TASKS,
// each marked done or to do, and says to do the first one not done now, and only it; with every one done it is the node's packet over
// the checkpoint that holds them. A plan with no sub-tasks has the packet it had before CRW-1459, whatever last.split is.
export function packet(node, plan, checkpoint, last) {
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
  if (Array.isArray(last)) {
    const n = notesSection(last)
    if (n) parts.push(n)
  } else {
    const was = last || {}
    for (const s of [
      findingsSection('LAST FINDINGS (the last unit check of this node found these in the work of a thread before yours; the work you hand over must answer each)', was.findings),
      ciSection('LAST VERIFICATION', was.ci),
      receiptSection('LAST RECEIPT (the summary of the last receipt of this node, from a thread you do not continue)', was.receipt),
    ]) if (s) parts.push(s)
  }
  const subs = plan.sub_tasks || []
  if (subs.length) {
    const split = (last && !Array.isArray(last) && last.split) || {}
    const done = split.done || 0
    const failed = split.failed || {}
    parts.push(section('SUB-TASKS (this node is split: do them one at a time, in this order, on this thread; the run commits each one before the next, and the node is judged once, after the last)',
      subs.map((sub, i) => '- ' + (i + 1) + '. ' + sub.id + ' (' + (i < done ? 'done' : 'to do') + '): ' + sub.summary +
        '\n  edit regions: ' + (sub.edit_regions || []).join(', ') + '\n  done condition: ' + sub.done_condition)))
    const f = failedSection(subs, failed)
    if (f) parts.push(f)
    parts.push(done < subs.length
      ? 'Do sub-task ' + (done + 1) + ' (' + subs[done].id + ') now, and only it: edit only its regions and answer with RECEIPT when it is done; the next sub-task comes as the next turn of this thread.'
      : 'Every sub-task is done and committed in your worktree: finish the node over them and answer with RECEIPT.')
  }
  if (checkpoint) {
    parts.push(section('CHECKPOINT (a fresh tier takes over; the worktree starts at this head)', [
      '- head: ' + checkpoint.head, '- tree: ' + checkpoint.tree,
    ]))
  }
  parts.push('Answer with RECEIPT when you are done. Edit files only under the absolute path P of your worktree.')
  return parts.join('\n\n')
}

// failedSection names the sub-tasks of a split node whose receipt reported failed (failed: their receipts by id), in the plan's order,
// each with its summary and gap; none, no section. Such a sub-task is committed with the rest and judged with the node. A receipt is
// read by the sub-task's own id: an id every object inherits a member of (constructor, a valid sub-task id) is not read through the
// prototype.
function failedSection(subs, failed) {
  const lines = []
  subs.forEach((sub, i) => {
    const r = Object.prototype.hasOwnProperty.call(failed, sub.id) ? failed[sub.id] : null
    if (r) lines.push('- ' + (i + 1) + '. ' + sub.id + ': ' + (r.summary || 'no summary') + (r.gap ? '\n  gap: ' + r.gap : ''))
  })
  if (lines.length === 0) return ''
  return section('REPORTED FAILED (an earlier sub-task of this node whose receipt reported failed; it is part of this node and is judged with it)', lines)
}

// subTaskPacket is the next turn of a split node's executor thread (CRW-1459): sub-task i (from 0) of the plan's sub-tasks, after the
// run committed the ones before it in the same worktree. failed are the receipts of the earlier sub-tasks that reported failed, by id
// (failedSection).
export function subTaskPacket(plan, i, failed) {
  const subs = plan.sub_tasks
  const sub = subs[i]
  const parts = [
    'ROLE: executor. Node ' + plan.node + ': sub-task ' + (i + 1) + ' of ' + subs.length + ' (' + sub.id + '): ' + sub.summary + '. ' +
      'The run committed the sub-tasks before it in your worktree; do this one now, and only it, in the same worktree, and answer with RECEIPT when it is done.',
    section('EDIT REGIONS', list(sub.edit_regions, 'undeclared')),
    section('DONE CONDITION', [sub.done_condition]),
  ]
  const f = failedSection(subs, failed || {})
  if (f) parts.push(f)
  return parts.join('\n\n')
}

export function answerPacket(answers) {
  return [
    'You asked for research. The answers follow; continue the same task.',
    section('ANSWERS', answers.map((a) => '- ' + a.question + '\n  ' + (a.answer || (a.route === 'inline' ? '(answer it yourself: the files are inside your regions or few)' : '(no answer)')))),
  ].join('\n\n')
}

// fixPacket is the next turn of the executor's thread when the run found something to fix: a failed verification (ci), or a verdict.
// The verdict is a unit check's (crw.unitCheck, CRW-1465: {verdict, findings}, each finding by its id, severity, file, line and text),
// which has no defects or criteria of a reviewer's VERDICT, or, on the old reviewer path an earlier run replays, a reviewer's VERDICT.
// notes are the explorer findings of that old path (the node lane passes none). The unit-check, verification and receipt sections are
// the helpers the executor's packet shares (findingsSection, ciSection, receiptSection), and the text is the one it was before them.
export function fixPacket(node, verdict, ci, notes, receipt) {
  const parts = ['ROLE: executor. Node ' + node + ': fix what the run found, in the same worktree, and answer with RECEIPT again.']
  if (ci && !ci.pass) parts.push(ciSection('VERIFICATION', ci))
  if (verdict && Array.isArray(verdict.findings) && !Array.isArray(verdict.defects)) {
    parts.push(section('UNIT CHECK (' + verdict.verdict + '; fix every finding)', findingLines(verdict.findings)))
    parts.push('A finding you cannot fix without an answer: answer needs_research with the question, and the run answers it on this thread.')
  } else if (verdict) {
    parts.push(section('REVIEW (' + verdict.verdict + ')', verdict.defects.map((d) => '- [' + d.severity + '/' + d.kind + '] ' + d.ref + ' ' + d.file + ':' + d.line + ' ' + d.finding)))
    parts.push(section('CRITERIA NOT MET', list(verdict.criteria.filter((c) => !c.met).map((c) => c.id + ': ' + c.evidence), 'none')))
  }
  const n = notesSection(notes)
  if (n) parts.push(n)
  const r = receiptSection('YOUR LAST RECEIPT', receipt)
  if (r) parts.push(r)
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

// CROSSCHECK_RULE is the one close rule of the crosscheck, word for word as ops.CrosscheckRule says it (the packet test compares them).
const CROSSCHECK_RULE = 'The impact of each finding is one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable: ' +
  'a finding of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block. ' +
  'The milestone closes only on a verified crosscheck: verified means no finding blocks; a finding that does not block is carried as a follow-up.'

// The lens of each pass of a crosscheck round.
const CROSSCHECK_LENS = {
  A: 'PASS A (coverage): judge every criterion and every design decision above, met or not, each with its evidence from the range (a file and line, or what you ran and saw). A criterion or a decision the range does not meet is a finding.',
  B: 'PASS B (bug hunt): hunt for the defects the range introduces - wrong results, broken edge cases, lost or exposed data, a permission boundary crossed, a regression of what worked before - each with its file and line.',
}

// The crosscheck pass (CRW-1473): the text of one pass of a crosscheck round over the integrated range of a milestone whose final gate
// passed. The host runs the review engine in Go (crw.crosscheck) and adds the EXECUTOR RECEIPTS section; the packet names the scope,
// the criteria, the decisions of the approved DESIGN, the lens of the pass and the close rule. opts ({prior, evidence}) is for the later
// rounds; round 1 reads neither.
export function crosscheckPacket(ms, g, pass, opts) {
  return [
    'CROSSCHECK pass ' + pass + ' of milestone ' + ms.id + ' (' + ms.title + '), after its final gate passed. Review the changes between ' + g.base + ' and ' + g.head + ': the integrated range, not the issues one by one. Your directory is a checkout of the head; read it and change no file.',
    'HEAD: ' + g.head + '\nBASE: ' + g.base + '\nRANGE: ' + g.range,
    section('CRITERIA OF THE MILESTONE', criteriaList(ms.criteria)),
    section('DESIGN DECISIONS (the approved DESIGN of this milestone)', decisionLines(g.decisions)),
    CROSSCHECK_LENS[pass] || CROSSCHECK_LENS.B,
    CROSSCHECK_RULE,
    'Answer with one JSON document: the verdict (verified or needs_changes) and the findings, each with its severity (P0 to P3), impact, file, line and text. A verdict of verified over a finding that blocks is refused as no answer.',
  ].join('\n\n')
}

// The fix packet of a crosscheck that needs changes (CRW-1473): what the 자식 gives a new executor subagent, and the route the fix
// takes. Every node of the milestone has landed, so the fix lands as a correction node of a new plan revision through land-head, and
// the verdict goes up to the 부모 with the landed head. The run itself calls no executor. The findings that block are those V does
// not carry.
export function crosscheckFixPacket(ms, v) {
  const carried = v.carried || []
  const blocking = (v.findings || []).filter((f) => !carried.includes(f.id))
  const files = []
  for (const f of blocking) if (f.file && !files.includes(f.file)) files.push(f.file)
  return [
    'CROSSCHECK FIX of milestone ' + ms.id + ' (' + ms.title + '): the crosscheck of round ' + v.round + ' at ' + v.head + ' needs changes, and the milestone stays open.',
    section('BLOCKING FINDINGS', list(blocking.map((f) => f.id + ' [' + f.severity + '/' + (f.impact || 'none') + '] ' + f.file + ':' + f.line + ' ' + f.text))),
    'RANGE: git diff ' + v.base + '..' + v.head + (files.length ? ' -- ' + files.join(' ') : ''),
    section('CRITERIA OF THE MILESTONE', criteriaList(ms.criteria)),
    section('ROUTE (every node of the milestone has landed: the fix is a correction node of a new plan revision)', [
      '- register a correction issue of milestone ' + ms.id + ' naming these finding ids: ' + (blocking.map((f) => f.id).join(', ') || 'none') + ' (crw-add-issue, a plan revision)',
      '- a new executor subagent fixes it on a branch from the integration tip',
      '- land the correction node with `crw flow start land-head`',
      '- then report this verdict and the landed head up to the 부모 (`crw flow report-up`)',
    ]),
  ].join('\n\n')
}

// The fix packet of a land-head run whose unit check needs changes (CRW-1490): what the 자식 gives a new executor subagent to fix the
// head it landed, and the land of the fix. The gate-only lane fixes nothing and the land gave the node's release back, so the fix is a
// new head made outside a run, landed by a new land-head run of the same node with the next fix round. n is the node, findings the
// check's findings (each by id, file, line and text), head the land's head ({commit, base}) and next the land of the fix ({plan,
// milestone, fix_round, authors}). The fix branches from this head, so the range of its land holds this head's work too: the authors of
// the fix's land are this land's authors and the fixer's thread, so the check stays independent of every author of the range.
export function landFixPacket(n, findings, head, next) {
  const authors = (next.authors || []).concat(['<the fixer subagent thread>'])
  const fixArgs = { plan: next.plan, milestone: next.milestone, issue: n.issue || n.id, head: '<the fixed head>', authors, fix_round: next.fix_round }
  return [
    'LAND FIX of node ' + n.id + ' (' + (n.issue || n.id) + (n.title ? ': ' + n.title : '') + '): the unit check of the committed range at ' + head.commit + ' needs changes, and the node did not land.',
    section('FINDINGS', findingLines(findings || [])),
    'RANGE: git diff ' + head.base + '..' + head.commit,
    section('CRITERIA OF THE NODE', criteriaList(n.criteria)),
    section('ROUTE (the gate-only lane fixes nothing; the land gave the node\'s release back)', [
      '- a new executor subagent fixes this head on a branch from it, outside any run',
      '- land it with `crw flow start land-head --args \'' + JSON.stringify(fixArgs) + '\' --handoff <the plan\'s handoff file>`, the fixed head, the fixer\'s thread and the handoff of the plan\'s current revision in place of the placeholders; keep the authors of this head',
    ]),
  ].join('\n\n')
}
