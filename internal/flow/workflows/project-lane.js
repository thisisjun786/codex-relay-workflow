import { RECEIPT, DESIGN, DESIGN_AUDIT, REFLECTION } from 'crw:lib/schemas'
import { install } from 'crw:lib/lane'
import { reportClose } from 'crw:lib/close'
import { answeredQuestions, architectPacket, auditFindings, designAnswersPacket, designAuditPacket, designFixPacket, reflectionPacket, replanPacket, packet, answerPacket, fixPacket, subTaskPacket } from 'crw:lib/packets'
export const meta = { name: 'project-lane', description: 'Run the issues of a milestone: background explorers, architect, design audit by the critic, explorers, one lane per issue node, milestone close and report', phases: ['explore', 'design', 'build', 'milestone'], args: { plan: 'string', milestone: 'string', cap: 'number' } }

// The project lane. Explorers first read the repository for each node of the milestone (the background questions, CRW-1466), the
// milestone is designed once by its architect over what they found, the DESIGN is audited by the critic in a separate review execution,
// before any executor starts, every issue node then runs its own lane (an executor, the unit check of its uncommitted work by a
// separately invoked review tool, run in Go with no reviewer agent or agent thread id, the run's commit and verification, the land). An observed same-thread review is warning-only and recorded, and the run continues; model family is provenance. The
// milestone is closed over what the lanes integrated. Every fact a lane uses comes from the journal: the plan of its issue from the
// design revision it was released under (crw.issuePlan), the evidence of its tier from the calls and checks it made. The operations
// are the host's (internal/flow/ops); the composites (crw.dag, crw.land, ...) come from crw:lib/lane.
install(crw)

// The caps of the design loop (one auditedDesign: the DESIGN it starts from, its audit rounds and the architect's fixes; CRW-1327).
// AUDIT_ROUNDS is the round cap: a DESIGN not approved in that many rounds stops the milestone with the design_audit_rounds
// escalation (CRW-1347). The token cap is DESIGN_TOKEN_CAP tokens, or DESIGN_TOKEN_WARN of the run's token budget when that is lower,
// so a run without a budget has one too: when the calls of the loop (its architect, answers, audit and fix calls and the explorers of its open
// questions) have used it, the loop warns once and tells the orchestrator (design_loop_tokens, no ask) and goes on; the engine itself
// pauses the run at the whole budget. The background explorers that run before the architect are outside the loop and its token cap
// (one explorer for each node bounds them; the engine's budget counts them like every call). Either cap writes a `warn design_loop_cap` log row. The run's budget is budget.run, the one its
// first process started with, which the journal fixes: a resume under another ceiling (budget.total) reaches the cap at the same
// call and raises the escalation with the same input, so the loop replays (CRW-1327).
const AUDIT_ROUNDS = 3
const DESIGN_TOKEN_CAP = 5000000
const DESIGN_TOKEN_WARN = 0.8

const ms = await crw.milestone(args.plan, args.milestone)
// the commands of the issues' done conditions (CRW-1382): the design-stage explorer runs each once and records the exit code and the tests
const END_CONDITIONS = []
for (const n of ms.nodes) for (const c of n.end_condition || []) END_CONDITIONS.push({ issue: n.issue, command: c })

// The design audit of a DESIGN of the architect: its open questions go to explorers or, when out of the architect's authority or
// beyond the explorer cap, to the orchestrator (crw.exploreRoute), the critic (a role of its own, in a separate review execution from the
// architect, CRW-1452) audits the DESIGN with the answers, after the answers an open question got have gone back to the architect on
// its own thread (design-answers, CRW-1466), and needs_changes sends it back to the architect on its own thread, for
// AUDIT_ROUNDS rounds. From the second round on the critic's packet carries the findings of the earlier rounds by id (r<round>.<index>)
// and the architect's disposition of each, from the DESIGN its design-fix answered (PRIOR FINDINGS). tag names
// the labels of a re-plan's rounds. Every call of the loop reads one head: a re-plan's context names its pinned head, and designHead
// is the head the first DESIGN's phase sampled (crw.designHead), which the first DESIGN's loop reads with no re-plan context. The answer is {ok, arch, answers}, or {ok: false, result} with what the milestone came to when
// the design could not be approved.
async function auditedDesign(first, tag, context, designHead) {
  let arch = first
  // the head every call of the loop reads: a re-plan's pinned head, or the design head the first DESIGN's phase sampled
  // (crw.designHead), so the open questions' explorers, the critic and the architect's turns judge the code the DESIGN was written for
  const at = (context && context.head) || designHead || undefined
  // the tokens of the loop are those its calls report (meta.tokens; crw.explore's tokens for its explorers), and its cap is made of
  // budget.run: both are the same on a resume that replays them, whatever ceiling the resume has
  const cap = budget.run === null ? DESIGN_TOKEN_CAP : Math.min(DESIGN_TOKEN_CAP, budget.run * DESIGN_TOKEN_WARN)
  const capText = budget.run === null ? 'the cap of DESIGN_TOKEN_CAP ' + DESIGN_TOKEN_CAP + ' tokens (the run has no budget)'
    : 'the cap of ' + cap + ' tokens (DESIGN_TOKEN_CAP ' + DESIGN_TOKEN_CAP + ', DESIGN_TOKEN_WARN ' + DESIGN_TOKEN_WARN + ' of the run budget of ' + budget.run + ')'
  let used = 0
  let warned = false
  // the findings of the rounds so far, by id, each round with the dispositions the architect's design-fix answer gave (null when it
  // gave none): the critic's PRIOR FINDINGS
  const prior = []
  async function tokenCap(tokens, round) {
    used += tokens || 0
    if (warned || used < cap) return
    warned = true
    const detail = 'tokens: the design loop ' + (tag ? tag + ' ' : 'of the milestone ') + 'used ' + used + ' tokens by audit round ' + round + ', ' + capText
    log('warn design_loop_cap ' + detail)
    await crw.escalate(null, { reason: 'design_loop_tokens', detail })
  }
  const tokensOf = (call) => (call && call.meta && call.meta.tokens && call.meta.tokens.total) || 0
  await tokenCap(tokensOf(first), 0)
  for (let round = 1; round <= AUDIT_ROUNDS; round++) {
    // the end condition commands of the issues are run once, by the first explorer of the first DESIGN's first round (CRW-1382)
    const ends = round === 1 && tag === '' ? END_CONDITIONS : []
    // a re-plan's explorers read the head it judges (CRW-1458); the first DESIGN's context has no head key, so its bytes are as before
    // the first DESIGN's open questions keep the reuse key of their text (no head in the route's context): every answer of its loop reads
    // the same design head; their explorers are placed at it (opts.head)
    const asked = await crw.explore('open_questions', arch.output.open_questions, context && context.head ? { milestone: ms.id, head: context.head } : { milestone: ms.id }, { endConditions: ends, head: at })
    await tokenCap(asked.tokens, round)
    // the authority questions of the round are asked now, approved or not (CRW-1366): an answer is the next round's
    await askOrchestrator(asked.answers, 'out_of_authority', asked.carried)
    if (asked.failed) return { ok: false, result: await crw.milestoneHalt(ms, 'explorer', asked.failed.status, asked.failed.reason, asked.failed.callId) }
    const answers = asked.answers
    // the answers go back to the architect on its own thread before the critic reads the DESIGN (CRW-1466): when a question of the
    // round got an answer, design-answers resumes the architect with them, and the DESIGN it answers with is the one audited (and the
    // handle a design-fix and the ledger resume); a question it raises there is routed in the next round
    if (answeredQuestions(answers).length) {
      arch = await agent.call(designAnswersPacket(answers, context), { resume: arch, schema: DESIGN, head: at, label: 'design-answers:' + tag + 'r' + round })
      if (arch.status !== 'completed') return { ok: false, result: await crw.milestoneHalt(ms, 'architect', arch.status, arch.reason, arch.callId) }
      await tokenCap(tokensOf(arch), round)
    }
    const cand = { design: arch.output, answers }
    await crw.spawnDecision('AUDIT', 'spawn', { role: 'critic', inputs: { milestone: ms.id, round, tag } })
    // the critic of a re-plan audits at the head the re-plan judges, in a read-only checkout of it, as the architect's turns are (CRW-1458)
    let call = await agent.call(designAuditPacket(ms, cand, round, prior, context), { agentType: 'critic', schema: DESIGN_AUDIT, head: at, label: 'design-audit:' + tag + 'r' + round })
    let audit = await crw.designAudit(ms.id, round, cand, arch, call)
    if (audit.state === 'null_model') {
      await tokenCap(tokensOf(call), round)
      call = await agent.call(designAuditPacket(ms, cand, round, prior, context), { agentType: 'critic', schema: DESIGN_AUDIT, head: at, label: 'design-audit:' + tag + 'r' + round + ':retry' })
      audit = await crw.designAudit(ms.id, round, cand, arch, call)
    }
    await tokenCap(tokensOf(call), round)
    if (audit.state === 'approved') return { ok: true, arch, answers, carried: asked.carried }
    if (audit.state !== 'needs_changes') return { ok: false, result: await crw.milestoneHalt(ms, 'design_audit', audit.state, audit.reason, call.callId) }
    // back to the architect, on its own thread: the DESIGN it revises is the one it wrote, and it disposes of each finding by its id
    arch = await agent.call(designFixPacket(audit, context, round), { resume: arch, schema: DESIGN, head: at, label: 'design-fix:' + tag + 'r' + round })
    if (arch.status !== 'completed') return { ok: false, result: await crw.milestoneHalt(ms, 'architect', arch.status, arch.reason, arch.callId) }
    const dispositions = arch.output.dispositions
    prior.push({ round, findings: auditFindings(round, audit.findings), dispositions: dispositions === undefined ? null : dispositions })
    await tokenCap(tokensOf(arch), round)
  }
  log('warn design_loop_cap rounds: the DESIGN ' + (tag ? tag + ' ' : 'of the milestone ') + 'was not approved in AUDIT_ROUNDS ' + AUDIT_ROUNDS + ' audit rounds')
  const esc = await crw.escalate(null, { reason: 'design_audit_rounds', detail: 'the DESIGN was not approved in ' + AUDIT_ROUNDS + ' audit rounds' })
  return { ok: false, result: { state: 'escalated', source: 'design_audit', reason: 'design_audit_rounds', escalation: esc.id } }
}

// The open questions routed to the orchestrator for reason are design questions for the orchestrator thread, each with its scope:
// one non-blocking ask, and the milestone goes on. The ones out of the architect's authority are asked in the audit round that
// routed them, the ones beyond the explorer cap of the milestone when the DESIGN is approved. The evidence names the digests asked:
// a question the run asked already is routed asked_in_run (crw.exploreRoute) and raises no ask of its own. Only the latest design
// ask of a run is current (the one crw flow answer accepts and the digest lists as waiting), so an ask carries, besides the
// questions it raises, every question of this DESIGN the run asked already and no answer has reached: asked_in_run, and the
// authority questions of the round when the approval asks the ones beyond the cap; and every question the run asked that no answer has
// reached and this DESIGN no longer raises (carried, from crw.exploreRoute, CRW-1381), so it stays answerable through the current ask.
async function askOrchestrator(answers, reason, carried) {
  const pending = (a) => a.route === 'orchestrator' && (a.reason === reason || a.reason === 'asked_in_run' || a.reason === 'out_of_authority')
  if (!answers.some((a) => a.route === 'orchestrator' && a.reason === reason)) return
  const asked = answers.filter(pending).concat(carried || [])
  await crw.escalate(null, { reason: 'design_question', detail: asked.map((a) => (a.scope ? '[' + a.scope + '] ' : '') + a.question).join(' | '), ask: 'design',
    evidence: { questions: asked.map((a) => a.digest) } })
}

// A re-plan (SP-ARCH-2): the architect's whole DESIGN of the milestone (CRW-1462: every decision and every issue of every node, the
// started ones as they were released, not a delta) is the next turn of its thread (the handle the ledger holds), asked at the head
// H in a read-only checkout of its own; it is audited again like the first DESIGN (audit rounds, and a return to the architect on the
// same thread for needs_changes), and only an approved DESIGN becomes the next revision of the ledger - the nodes that have not
// started are released under a revision only after that. signal is the number of the signal that made the re-plan (0 at the close);
// unstarted is the ids of the nodes that have not started (none at the close), which the architect and the critic both read.
async function replan(tag, signal, due, head, misaligned, unstarted) {
  const cur = await crw.currentDesign(ms.id)
  await crw.spawnDecision('ARCH_2', 'spawn', { role: 'architect', reason: due.reasons.join(','), inputs: {
    milestone: ms.id, revision: cur.revision, signal, head, reasons: due.reasons, plan_mismatch: due.plan_mismatch, slice_change: due.slice_change.changed } })
  // a re-plan after an imported DESIGN starts a fresh local architect on the reused design's context (CRW-1329): the foreign
  // run's thread handle is never resumed
  const call = await agent.call(replanPacket(ms, cur, due, head, misaligned, unstarted), Object.assign({ schema: DESIGN, head, label: 'replan:' + tag }, cur.handle ? { resume: cur.handle } : { agentType: 'architect' }))
  if (call.status !== 'completed') return { state: 'halted', result: await crw.milestoneHalt(ms, 'replan', call.status, call.reason, call.callId) }
  // the plans the started nodes were released with: the critic compares the whole DESIGN with them (RELEASED PLANS)
  const released = await crw.releasedPlans(ms.id)
  const audited = await auditedDesign(call, 'replan-' + tag + ':', { head, base_tip: cur.base_tip, landed: cur.landed, unstarted, released })
  if (!audited.ok) return { state: 'halted', result: audited.result }
  const led = await crw.designLedger(ms.id, { design: audited.arch.output, answers: audited.answers, architect: audited.arch, signal, head, plan: due.plan })
  await askOrchestrator(audited.answers, 'explorer_cap', audited.carried)
  return { state: 'revised', revision: led.revision }
}

// What a signal of the dag comes to: the numbers of SP-ARCH-2 are counted and, when a reason stands and a node has not started, the
// milestone re-plans; the signal is then a refresh (the nodes that have not started are released under the new revision) or, when the
// re-plan could not be done, a halt. Every decision, the ones to skip included, is a row of flow_spawn_decisions.
async function onSignal(sig) {
  const due = await crw.replanSignals(ms.id, { phase: 'signal', signal: sig.k })
  const inputs = { milestone: ms.id, signal: sig.k, revision: due.revision, plan_mismatch: due.plan_mismatch, slice_change: due.slice_change.changed, unstarted: sig.unstarted }
  if (!due.due) {
    await crw.spawnDecision('ARCH_2', 'skip', { reason: 'no_replan_signal', inputs })
    return 'continue'
  }
  if (sig.unstarted.length === 0) {
    // nothing is left to release under another revision: the close judges the same numbers
    await crw.spawnDecision('ARCH_2', 'skip', { reason: 'no_unstarted_node', inputs: Object.assign({ reasons: due.reasons }, inputs) })
    return 'continue'
  }
  // the re-plan names the nodes the signal found unstarted: the ones its revision releases
  const out = await replan('s' + sig.k, sig.k, due, sig.tip, null, sig.unstarted)
  if (out.state !== 'revised') return { result: 'halt', reasons: due.reasons }
  // the nodes that have not started are admitted by the regions of the revision just made, not of the first one
  const cur = await crw.currentDesign(ms.id)
  const regions = {}
  for (const is of cur.design.issues) {
    const planned = ms.nodes.find((n) => n.id === is.node)
    regions[is.node] = await crw.admissionRegions(planned ? (planned.planned_regions || planned.regions) : [], is.edit_regions)
  }
  return { result: 'refresh', revision: out.revision, reasons: due.reasons, regions }
}

// The background questions (CRW-1466, SP-EXPL-1): before the architect's first DESIGN, one question for each node of the milestone goes
// to an explorer, which reads what the repository holds for it today; the architect's packet carries the answers (BACKGROUND
// FINDINGS). A question names the node's id, its issue key and its title, so its digest is the node's and never an open question's;
// its files_needed are the node's declared regions (none declared: not named). crw.exploreRoute spawns each or reuses an answer of the
// same digest the run has, never inline, never to the orchestrator and outside the open questions' cap. Only the first DESIGN has this
// phase: a re-plan is the next turn of the architect's thread, which has read them. The end condition commands stay with the first
// open-question explorer of the design loop.
function backgroundQuestion(n) {
  return 'Background of node ' + n.id + ' (' + n.issue + '): ' + n.title + '. What does the repository hold today that the plan of this issue rests on: the files, types, functions and tests it will change or read, the conventions they follow, and what in them must not change?'
}
// the DESIGN of an earlier run of this plan and repository is reused when crw.designReuse finds it approved over the same slices
// and milestone digest (CRW-1329): the run's design phase is skipped and its ledger's first revision imports it
const reused = await crw.designReuse(ms.id)
let initialDesign
if (reused.reused) {
  phase('design')
  await crw.spawnDecision('ARCH_1', 'skip', { role: 'architect', reason: 'approved_design_reused', inputs: { milestone: ms.id, source_run: reused.source_run, source_revision: reused.source_revision, digest: reused.digest } })
  await crw.designLedger(ms.id, { reuse: true })
  initialDesign = reused.design
} else {
  phase('explore')
  // one design head for the phase (crw.designHead): every background explorer and the architect's first call read it, whatever lands on
  // the integration target meanwhile, and the architect's checkout, and so the head r0 records the DESIGN ran at, is made at it
  const designAt = await crw.designHead(ms.id)
  const background = await crw.explore('background', ms.nodes.map((n) => ({ id: 'bg-' + n.id, question: backgroundQuestion(n), scope: '', files_needed: n.regions.length ? n.regions : null })), { milestone: ms.id, head: designAt.head }, {})
  if (background.failed) return crw.milestoneHalt(ms, 'explorer', background.failed.status, background.failed.reason, background.failed.callId)

  phase('design')
  await crw.spawnDecision('ARCH_1', 'spawn', { role: 'architect', inputs: { milestone: ms.id, nodes: ms.nodes.length } })
  const first = await agent.call(architectPacket(ms, background.answers, designAt.head), { agentType: 'architect', schema: DESIGN, head: designAt.head, label: 'architect' })
  if (first.status !== 'completed') return crw.milestoneHalt(ms, 'architect', first.status, first.reason, first.callId)
  const designed0 = await auditedDesign(first, '', undefined, designAt.head)
  if (!designed0.ok) return designed0.result
  const arch = designed0.arch
  await crw.designLedger(ms.id, { design: arch.output, answers: designed0.answers, architect: arch })
  await askOrchestrator(designed0.answers, 'explorer_cap', designed0.carried)
  initialDesign = arch.output
}

// A fenced write after another session claimed the plan's coordinator epoch is refused epoch_lost (CRW-1285): the lane ends at that
// write with the reason, and neither escalates nor writes anything more at the relay; the run ends stopped on it (below).
const EPOCH_LOST = 'epoch_lost'
const epochLost = (e) => /refused:epoch_lost:/.test((e && e.message) || '')
async function lane(n, rel, sig) {
  try {
    return await nodeLane(n, rel, sig)
  } catch (e) {
    if (epochLost(e)) return { node: n.id, state: 'stopped', reason: EPOCH_LOST, detail: e.message }
    throw e
  }
}

// The lane of one issue node. It reads no variable of the script that another lane may have changed: its facts are the plan of
// its release revision and the results of its own calls.
//
// The node's work is judged by its unit check before the run commits it (CRW-1465): crw.unitCheck runs the review tool in Go over the
// executor's uncommitted changes (no reviewer agent is called). A check with any finding goes back to the same executor thread to be
// fixed, at most UNIT_CHECK_ROUNDS round trips on a tier; the findings of a check after them are carried, before any tier step: only
// non-blocking ones and the node lands with them (premerge defects that need no disposition: crw.land hands their ids and the checks
// that carried them to crw.accept, and `carried` is in the lane result), a blocking one and the node escalates (ask land, unit_check_carried) with the carried ids.
// While round trips are left, the tier evidence (SP-TIER-1, where only a check that rules needs_changes is a reviewed round with a
// finding) steps the tier before a fix. A check the tool gave no judgment in (quota, no answer) or that had no executor receipt to hand
// over holds the node (crw.hold, source unit_check), and a resume runs the check again; any other check that judged nothing (the
// worktree or its HEAD moved, the tool failed, an invalid answer twice) escalates as a configuration question. The checked tree is then
// committed, verified and landed; a failed verification goes back to the same thread and its fix is checked again. So a commit holds
// only checked work: a fresh thread (a tier step, a rebuild) starts at the node's last commit, and the uncommitted trees of the attempt
// it leaves are abandoned there (the packet carries their last findings and receipt); a resume replays the calls that made them, whose
// effect nothing reads again (ops.Prover).
const UNIT_CHECK_ROUNDS = 2
async function nodeLane(n, rel, sig) {
  const node = n.id
  const plan = await crw.issuePlan(node)
  if (plan.size_fit === 'child') {
    // the issue-child path is retired (CRW-1415): a node sized for a child goes up as a size split, and no child is started
    const esc = await crw.escalate(node, { reason: 'size_split', detail: 'child_retired', evidence: { size_reason: plan.size_reason } })
    return { node, state: 'escalated', reason: 'size_split', escalation: esc.id }
  }
  if (plan.size_fit === 'bundle_split') {
    // a node too big for one node goes up to the parent as a bundle split request (CRW-1437): the size_split escalation closes its
    // execution and returns its slot before any executor call, and no decision is waited for
    const esc = await crw.escalate(node, { reason: 'size_split', detail: plan.size_reason, ask: 'bundle_split_request', evidence: { size_reason: plan.size_reason } })
    return { node, state: 'escalated', reason: 'size_split', escalation: esc.id }
  }
  if (plan.size_fit === 'split' && !(plan.sub_tasks && plan.sub_tasks.length)) {
    // a split of a DESIGN recorded before S-1 names no sub-tasks (CRW-1435): no executor was released for it, it goes up as a size split
    const esc = await crw.escalate(node, { reason: 'size_split', detail: plan.size_reason })
    return { node, state: 'escalated', reason: 'size_split', escalation: esc.id }
  }
  const ev = crw.evidence(node)
  let tier = 'executor'
  let step = null
  // the unit-check fix round trips of the tier
  let fixes = 0
  // the finding ids carried past the last round trip, kept to the land: a later check of a verification fix judges only that fix, so
  // crw.land hands them to the acceptance with the check that carried each (carriedBy), which reads it from that check's record into
  // the premerge record
  let carried = []
  const carriedBy = {}
  let ex = null
  let head = null
  // the node's last state (CRW-1469): the findings of its last unit check (none after a check that found nothing), its last
  // verification and the summary of its last receipt, all from the node's own calls and checks. A fresh thread has none of the turns
  // before it, so its packet carries them; the first build has none.
  const last = { findings: [], ci: null, receipt: null }
  // the sub-tasks of a node sized for a split (CRW-1459) run in their order on one executor thread: done is the sub-tasks with a
  // checkpoint commit of the node, from the first, and failed the receipts of those that reported failed, by id. A sub-task before the
  // last is committed as a checkpoint, with no unit check, no verification and no land, and the same thread goes on to the next; the
  // receipt of the last, and every one after it, goes on to the node's commit and verification, then the one check of the node's whole
  // change (base..head, every criterion of the node) and the land, so the node is judged once, as a whole, and its acceptance rests on
  // that check alone. A fresh thread (a rebuild, the executor-t1 build) starts at the node's last commit, which holds the done ones, and
  // its packet starts at the first one not done: the progress is part of the node's last state (last.split), which a fresh thread's
  // packet carries.
  const subs = (plan.size_fit === 'split' && plan.sub_tasks) || []
  const split = { done: 0, failed: {} }
  if (subs.length) last.split = split

  // a fresh executor thread of the tier: the first build, the rebuild after a refusal or a null_model, the executor-t1 build
  async function start(label, checkpoint) {
    await crw.spawnDecision('EXEC_1', 'spawn', { node, role: tier, tier, inputs: { label } })
    const opts = { agentType: tier, node, schema: RECEIPT, label }
    if (step && tier === 'executor-t1') opts.stepDecision = step.id
    return agent.call(packet(node, plan, checkpoint, last), opts)
  }
  // what a unit check of the round comes to: end (the lane's result: a hold, a check that judged nothing, carried findings that
  // block), fix (a thunk that sends every finding back to the executor's own thread, one round trip), step (the tier evidence stands
  // while round trips are left: the executor-t1 starts at the node's last commit, which holds only checked work, and the tree whose
  // findings are not answered stays in the attempt the step leaves), or none of them (no finding, or carried ones that do not block)
  async function judge(uc, round) {
    if (uc.hold) {
      // the held call is the one whose receipt was not taken: the judged call, or another executor call of a committed check's range
      const held = uc.call || ex.callId
      await crw.hold(node, { reason: uc.hold, call: held, source: 'unit_check' })
      return { end: { node, state: 'held', reason: uc.hold, source: 'unit_check', call: held } }
    }
    if (uc.state !== 'ok') {
      const esc = await crw.escalate(node, { reason: 'unit_check_' + uc.state, call: ex.callId, ask: 'configuration', detail: uc.detail || '' })
      return { end: { node, state: 'escalated', reason: 'unit_check_' + uc.state, call: ex.callId, escalation: esc.id } }
    }
    const found = uc.findings || []
    last.findings = found
    ev.unitCheck(tier, uc.check_id, uc.verdict, found.map((f) => f.id))
    if (found.length === 0) return {}
    if (fixes >= UNIT_CHECK_ROUNDS) {
      // the findings after the last round trip are carried, before any tier step: a blocking one does not land
      for (const f of found) {
        if (!carried.includes(f.id)) {
          carried.push(f.id)
          carriedBy[f.id] = uc.check_id
        }
      }
      if (uc.verdict !== 'verified') {
        const esc = await crw.escalate(node, { reason: 'unit_check_carried', call: ex.callId, ask: 'land', evidence: { carried, check: uc.check_id },
          detail: found.map((f) => f.id + ' ' + f.file + ' ' + f.text).join(' | ') })
        return { end: { node, state: 'escalated', reason: 'unit_check_carried', call: ex.callId, carried, escalation: esc.id } }
      }
      return {}
    }
    if (ev.tierStepDue(tier)) return { step: true }
    const label = 'fix:' + node + ':' + tier + ':r' + round
    return {
      fix: () => {
        fixes++
        return agent.call(fixPacket(node, { verdict: uc.verdict, findings: found }, null, null, ex.output), { resume: ex, schema: RECEIPT, label })
      },
    }
  }

  // the options of the node's land: the head, its verification and the findings carried past the last round trip, each with the check
  // that carried it
  function landOpts(head, ci) {
    return carried.length ? { head, ci, carried: carried.map((id) => ({ id, check: carriedBy[id] })) } : { head, ci }
  }
  // what the land of the node comes to: the lane's result
  async function landEnd(landed) {
    if (landed.state === 'landed') return carried.length ? Object.assign(landed, { carried }) : landed
    if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail }
    const esc = await crw.escalate(node, { reason: landed.reason || 'land_refused', detail: landed.detail || '', ask: 'land' })
    return { node, state: 'escalated', reason: landed.reason, escalation: esc.id }
  }

  ex = await start('build:' + node + ':' + tier, null)

  for (let round = 1; ; round++) {
    const s = crw.classify(ex)
    // the RECEIPT of the call is a trigger of the signal path
    sig.receipt(ex)
    if (ex.output && ex.output.summary) last.receipt = { summary: ex.output.summary }
    ev.executor(tier, ex.callId, s)
    if (s === 'superseded') return { node, state: 'superseded', call: ex.callId }
    if (s === 'research') {
      // SP-EXPL-2: the question is answered and the SAME executor thread goes on; a research request is never a tier step. The
      // research continuations of the tier are counted by crw.exploreRoute alone (spawned and reused alike, CRW-1470): past them
      // the route is escalate, and its reason (research_cap) is the detail
      const asked = await crw.explore('research', [{ id: ex.callId, question: ex.output.research_question, scope: '', files_needed: null }], { node, tier, regions: plan.regions })
      if (asked.failed) {
        await crw.hold(node, { reason: asked.failed.reason || asked.failed.status, call: asked.failed.callId, source: 'explorer' })
        return { node, state: 'held', reason: asked.failed.reason, call: asked.failed.callId }
      }
      if (asked.escalate.length > 0) {
        const esc = await crw.escalate(node, { reason: 'blocked_scope', call: ex.callId, detail: asked.escalate[0].reason })
        return { node, state: 'escalated', reason: 'blocked_scope', call: ex.callId, escalation: esc.id }
      }
      ex = await agent.call(answerPacket(asked.answers), { resume: ex, schema: RECEIPT, label: 'research:' + node + ':' + tier + ':r' + round })
      continue
    }
    if (s === 'scope') {
      const esc = await crw.escalate(node, { reason: 'blocked_scope', call: ex.callId, detail: ex.output.gap || '' })
      return { node, state: 'escalated', reason: 'blocked_scope', call: ex.callId, escalation: esc.id }
    }
    if (s === 'refused') {
      // a configuration fault (pair, policy, schema, host configuration): the orchestrator decides, and only an applied decision
      // goes on, with a fresh thread of the same tier
      const esc = await crw.escalate(node, { reason: ex.reason, call: ex.callId, ask: 'configuration' })
      const d = await crw.decide(node, { id: esc.id })
      if (!d.applied) return { node, state: 'waiting', reason: ex.reason, call: ex.callId, escalation: esc.id }
      ex = await start('rebuild:' + node + ':' + tier + ':r' + round, step && step.checkpoint)
      continue
    }
    if (s === 'null_unknown') {
      await crw.hold(node, { reason: 'dispatch_unknown', call: ex.callId })
      return { node, state: 'held', reason: 'dispatch_unknown', call: ex.callId }
    }
    if (s === 'host_held') {
      // infrastructure, quota, budget or an engine pause: never a new thread and never tier evidence
      await crw.hold(node, { reason: ex.reason, call: ex.callId })
      return { node, state: 'held', reason: ex.reason, call: ex.callId }
    }
    if (s === 'null_model') {
      if (!ev.tierStepDue(tier)) {
        // the first null_model: a fresh thread of the same tier, in a new attempt worktree at the node's last commit
        ex = await start('rebuild:' + node + ':' + tier + ':r' + round, step && step.checkpoint)
        continue
      }
    } else if (subs.length) {
      // ready or self_failed of a split node: the run judges the worktree, never the executor's report
      if (split.done + 1 < subs.length) {
        // a sub-task before the last: a checkpoint commit of its tree against its own regions, with no unit check, and the next
        // sub-task on the same thread; a sub-task that reported failed is no judgement of the node, which judges it with the rest after
        // the last
        const sub = subs[split.done]
        await crw.commit(node, ex, sub.edit_regions)
        if (s === 'self_failed') split.failed[sub.id] = { summary: (ex.output && ex.output.summary) || '', gap: (ex.output && ex.output.gap) || '' }
        split.done++
        ex = await agent.call(subTaskPacket(plan, split.done, split.failed), { resume: ex, schema: RECEIPT, label: 'subtask:' + node + ':' + tier + ':s' + (split.done + 1) })
        continue
      }
      // the last sub-task, a fix after it or a tier's build: the node's commit, which holds every sub-task, with no check before it
      head = await crw.commit(node, ex, plan.regions)
      if (split.done < subs.length) split.done = subs.length
      const ci = await crw.verify(node, head)
      if (!ci.ran) {
        await crw.hold(node, { reason: 'verify_unavailable', call: ex.callId, source: 'verify' })
        return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
      }
      ev.verification(tier, head.commit, ci.pass, ci.digest)
      last.ci = ci
      if (ci.pass) {
        // a split node is judged as a whole (CRW-1459): the committed range from the node's base (the base its verification stands on)
        // to its head is checked against every criterion of the node, and the acceptance rests on this check alone; its findings go to
        // the same thread as a fix round, whose work is committed, verified and checked as a whole again
        const whole = await judge(await crw.unitCheck(node, ex, { round, final: fixes >= UNIT_CHECK_ROUNDS, commit: head, base: ci.base }), round)
        if (whole.end) return whole.end
        if (whole.fix) {
          ex = await whole.fix()
          continue
        }
        if (!whole.step) return landEnd(await crw.land(node, landOpts(head, ci)))
      } else if (!ev.tierStepDue(tier)) {
        // fix the verification on the executor's own thread; its work is committed, verified and checked as a whole again
        ex = await agent.call(fixPacket(node, null, ci, null, ex.output), { resume: ex, schema: RECEIPT, label: 'fix:' + node + ':' + tier + ':r' + round })
        continue
      }
    } else {
      // ready or self_failed: the run judges the worktree, never the executor's report. The unit check reads the uncommitted
      // changes the call left (the tree crw.commit will commit); final: no fix round can follow it on this tier.
      const uc = await judge(await crw.unitCheck(node, ex, { round, final: fixes >= UNIT_CHECK_ROUNDS }), round)
      if (uc.end) return uc.end
      if (uc.fix) {
        ex = await uc.fix()
        continue
      }
      if (!uc.step) {
        head = await crw.commit(node, ex, plan.regions)
        const ci = await crw.verify(node, head)
        if (!ci.ran) {
          await crw.hold(node, { reason: 'verify_unavailable', call: ex.callId, source: 'verify' })
          return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
        }
        ev.verification(tier, head.commit, ci.pass, ci.digest)
        last.ci = ci
        if (ci.pass) return landEnd(await crw.land(node, landOpts(head, ci)))
        if (!ev.tierStepDue(tier)) {
          // fix the verification on the executor's own thread; its work is checked again before the next commit
          ex = await agent.call(fixPacket(node, null, ci, null, ex.output), { resume: ex, schema: RECEIPT, label: 'fix:' + node + ':' + tier + ':r' + round })
          continue
        }
      }
    }
    // SP-TIER-1 evidence stands on the tier: one step to executor-t1, and no step after it
    if (tier === 'executor-t1') {
      await crw.spawnDecision('TIER_2', 'refused', { node, role: tier, tier, reason: 'tiers_exhausted', inputs: ev.evidence(tier) })
      const esc = await crw.escalate(node, { reason: 'tiers_exhausted', evidence: ev.evidence(tier) })
      return { node, state: 'escalated', reason: 'tiers_exhausted', escalation: esc.id }
    }
    step = await crw.tierStep(node, ev.evidence(tier))
    tier = 'executor-t1'
    fixes = 0
    ex = await start('build:' + node + ':' + tier, step.checkpoint)
  }
}

phase('build')
// A node is admitted by the regions of the plan and of the approved DESIGN together (crw.admissionRegions, the Go op): the executor of a
// node edits the DESIGN's edit_regions, so the nodes that run at once must not overlap in those either. A planned region keeps the plan's
// change and hold (planned_regions, when the plan states one that is not a plain edit), which crw.regionsOverlap judges by (CRW-1461).
const designed = {}
for (const is of initialDesign.issues) designed[is.node] = is.edit_regions
const admitted = []
for (const n of ms.nodes) admitted.push(Object.assign({}, n, { regions: await crw.admissionRegions(n.planned_regions || n.regions, designed[n.id]) }))
const lanes = await crw.dag(args.plan, lane, { nodes: admitted, milestone: ms.id, cap: args.cap, onSignal })
const lost = lanes.filter((l) => l && l.reason === EPOCH_LOST)
if (lost.length > 0) return { state: 'stopped', reason: EPOCH_LOST, detail: (lost.find((l) => l.detail) || lost[0]).detail || '', milestone: ms.id, lanes }
// the run-level hold plan_superseded (CRW-1291): the relay's plan moved past the revision the run pinned, and the run holds for its parent
if (lanes.some((l) => l && l.state === 'held' && l.reason === 'plan_superseded')) return { state: 'held', reason: 'plan_superseded', milestone: ms.id, lanes }
if (!lanes.every((l) => l && l.state === 'landed')) return { state: 'incomplete', milestone: ms.id, lanes }

// A fenced write the steps after the lanes make (the close and the re-plan) once another session claimed the plan's epoch ends the run
// stopped with the reason too (CRW-1382), as a lane that meets it does: closeAndReport() runs those steps and the catch below takes the error kind.
try {
  return await closeAndReport()
} catch (e) {
  if (epochLost(e)) return { state: 'stopped', reason: EPOCH_LOST, detail: e.message, milestone: ms.id, lanes }
  throw e
}

async function closeAndReport() {
  phase('milestone')
  // The close: the final gate over the integrated head (test evidence, and the QA receipt for a driven surface), the crosscheck over the
  // range with the passed gate as its input (two passes of a review engine in a separate tool invocation, run by the host; no agent thread id),
  // and the close recorded over the tip it judged. A gate that fails or cannot be settled ends the milestone here, and the crosscheck does
  // not run; a crosscheck that does not verify the range ends it too, and nothing is closed, reflected on or reported (crw:lib/close).
  // The halt carries what the close's stop does: the ask, the crosscheck's V in the escalation's evidence and in the run's value, and the
  // fix packet the 자식 hands on.
  const closed = await crw.closeMilestone(ms)
  if (closed.state !== 'closed') return crw.milestoneHalt(ms, closed.source, closed.status, closed.reason, closed.call, { ask: closed.ask, evidence: closed.evidence, fix: closed.fix, detail: closed.detail })
  // The architect's reflection (SP-ARCH-3): the next turn of its thread, at the integrated head the gate pinned, in a read-only checkout
  // of it. A call that did not complete is not ALIGNED: held and unknown hold the milestone, everything else escalates.
  const current = await crw.currentDesign(ms.id)
  await crw.spawnDecision('ARCH_3', 'spawn', { role: 'architect', inputs: { milestone: ms.id, revision: current.revision, head: closed.gate.head } })
  const reflected = await agent.call(reflectionPacket(ms, current, closed), Object.assign({ schema: REFLECTION, head: closed.gate.head, label: 'reflection:' + ms.id }, current.handle ? { resume: current.handle } : { agentType: 'architect' }))
  const reflection = await crw.reflection(ms.id, closed.gate, reflected)
  if (reflection.state !== 'verdict') return crw.milestoneHalt(ms, 'reflection', reflection.state, reflection.reason, reflected.callId)
  // A reason to re-plan stands whatever the alignment says: the numbers of the signals, and the alignment the reflection found.
  const due = await crw.replanSignals(ms.id, { phase: 'close', reflection: reflection.alignment })
  // The closed milestone is audited and reported before any re-plan: the acceptances are audited against the records they rest on, and
  // the report carries the audit, the gate, the crosscheck and the reflection.
  const reported = await reportClose(ms, closed, reflection.alignment)
  if (due.due) {
    // at the close every node has run: the whole DESIGN names none unstarted
    const out = await replan('close', 0, due, closed.gate.head, reflection.misaligned, [])
    if (out.state !== 'revised') return out.result
    // the correction issues are the orchestrator's: crw-add-issue and a plan revision, planned in the revision the architect just made
    const esc = await crw.escalate(null, { reason: 'replan_correction', detail: 'revision ' + out.revision + ' of milestone ' + ms.id + ' after ' + due.reasons.join(', '), ask: 'correction_issues' })
    return { state: 'replanned', milestone: ms.id, lanes, tip: closed.tip, revision: out.revision, reasons: due.reasons, escalation: esc.id, report: reported.report.report_id,
      crosscheck: closed.crosscheck.verdict, reflection: reflection.alignment }
  }
  await crw.spawnDecision('ARCH_2', 'skip', { reason: 'no_replan_signal', inputs: { milestone: ms.id, revision: due.revision, plan_mismatch: due.plan_mismatch, slice_change: due.slice_change.changed, reflection: reflection.alignment } })
  return { state: 'closed', milestone: ms.id, lanes, tip: closed.tip, report: reported.report.report_id, gate: closed.gate.state, crosscheck: closed.crosscheck.verdict, reflection: reflection.alignment,
    audit: { ok: reported.audit.ok, mismatch: reported.audit.mismatch, unobserved: reported.audit.unobserved } }
}
