import { RECEIPT, DESIGN, DESIGN_AUDIT } from 'crw:lib/schemas'
import { install } from 'crw:lib/lane'
import { architectPacket, designAuditPacket, designFixPacket, packet, answerPacket, fixPacket } from 'crw:lib/packets'
export const meta = { name: 'project-lane', description: 'Run the issues of a milestone: architect, design audit, explorers, one lane per issue node, milestone close and report', phases: ['design', 'build', 'milestone'], args: { plan: 'string', milestone: 'string', cap: 'number' } }

// The project lane. A milestone is designed once by its architect, the DESIGN is audited by a reviewer of another model family
// before any executor starts, every issue node then runs its own lane (an executor, the run's commit and verification, an
// independent review, the land), and the milestone is closed over what the lanes integrated. Every fact a lane uses comes from
// the journal: the plan of its issue from the design revision it was released under (crw.issuePlan), the evidence of its tier
// from the calls it made. The operations are the host's (internal/flow/ops); the composites (crw.dag, crw.review, crw.land, ...)
// come from crw:lib/lane.
install(crw)

const AUDIT_ROUNDS = 3
// the research continuations a node gets on one tier (SP-EXPL-2); after them the node escalates as a scope question
const RESEARCH_PER_TIER = 2

// A milestone that cannot go on: a call of the design phase that did not complete. held and unknown hold it (a resume goes on
// when the cause is gone), refused and null_model escalate, superseded stops. A call that did not complete is never read as an
// approval or as a design.
async function halt(ms, source, status, reason, callId) {
  const node = 'milestone:' + ms.id
  if (status === 'held' || status === 'unknown') {
    await crw.hold(node, { reason: status === 'unknown' ? 'dispatch_unknown' : (reason || 'host_infra_unknown'), call: callId, source })
    return { state: 'held', source, reason, call: callId }
  }
  if (status === 'superseded') return { state: 'superseded', source, call: callId }
  const esc = await crw.escalate(null, { reason: source + '_' + status, call: callId, detail: reason || '' })
  return { state: 'escalated', source, reason: reason || status, call: callId, escalation: esc.id }
}

const ms = await crw.milestone(args.plan, args.milestone)

phase('design')
await crw.spawnDecision('ARCH_1', 'spawn', { role: 'architect', inputs: { milestone: ms.id, nodes: ms.nodes.length } })
let arch = await agent.call(architectPacket(ms), { agentType: 'architect', schema: DESIGN, label: 'architect' })
if (arch.status !== 'completed') return halt(ms, 'architect', arch.status, arch.reason, arch.callId)

let answers = []
let audited = false
for (let round = 1; round <= AUDIT_ROUNDS && !audited; round++) {
  const asked = await crw.explore('open_questions', arch.output.open_questions, { milestone: ms.id })
  if (asked.failed) return halt(ms, 'explorer', asked.failed.status, asked.failed.reason, asked.failed.callId)
  answers = asked.answers
  const cand = { design: arch.output, answers }
  await crw.spawnDecision('AUDIT', 'spawn', { role: 'reviewer', inputs: { milestone: ms.id, round } })
  let call = await agent.call(designAuditPacket(ms, cand, round), { agentType: 'reviewer', schema: DESIGN_AUDIT, label: 'design-audit:r' + round })
  let audit = await crw.designAudit(ms.id, round, cand, arch, call)
  if (audit.state === 'null_model') {
    call = await agent.call(designAuditPacket(ms, cand, round), { agentType: 'reviewer', schema: DESIGN_AUDIT, label: 'design-audit:r' + round + ':retry' })
    audit = await crw.designAudit(ms.id, round, cand, arch, call)
  }
  if (audit.state === 'approved') {
    audited = true
  } else if (audit.state === 'needs_changes') {
    // back to the architect, on its own thread: the DESIGN it revises is the one it wrote
    arch = await agent.call(designFixPacket(audit), { resume: arch, schema: DESIGN, label: 'design-fix:r' + round })
    if (arch.status !== 'completed') return halt(ms, 'architect', arch.status, arch.reason, arch.callId)
  } else {
    return halt(ms, 'design_audit', audit.state, audit.reason, call.callId)
  }
}
if (!audited) {
  const esc = await crw.escalate(null, { reason: 'design_audit_rounds', detail: 'the DESIGN was not approved in ' + AUDIT_ROUNDS + ' audit rounds' })
  return { state: 'escalated', source: 'design_audit', reason: 'design_audit_rounds', escalation: esc.id }
}
const ledger = await crw.designLedger(ms.id, { revision: 'r0', design: arch.output, answers, architect: arch })
// the questions beyond the explorer cap of the milestone are design questions for the orchestrator thread; the milestone goes on
const asked = answers.filter((a) => a.route === 'orchestrator')
if (asked.length > 0) await crw.escalate(null, { reason: 'design_question', detail: asked.map((a) => a.question).join(' | '), ask: 'design' })

// The lane of one issue node. It reads no variable of the script that another lane may have changed: its facts are the plan of
// its release revision and the results of its own calls.
async function lane(n, rel) {
  const node = n.id
  const plan = await crw.issuePlan(node)
  if (plan.size_fit === 'child') {
    await crw.spawnDecision('CHILD_2', 'spawn', { node, role: 'child', reason: plan.size_reason })
    const child = await crw.releaseChild(node, plan)
    return { node, state: 'child', child: child.child }
  }
  if (plan.size_fit === 'split') {
    const esc = await crw.escalate(node, { reason: 'size_split', detail: plan.size_reason })
    return { node, state: 'escalated', reason: 'size_split', escalation: esc.id }
  }
  const ev = crw.evidence(node)
  let tier = 'executor'
  let step = null
  let notes = []
  let researched = 0
  let ex = null
  let head = null

  // a fresh executor thread of the tier: the first build, the rebuild after a null_model, the executor-t1 build
  async function start(label, checkpoint) {
    await crw.spawnDecision('EXEC_1', 'spawn', { node, role: tier, tier, inputs: { label } })
    const opts = { agentType: tier, node, schema: RECEIPT, label }
    if (step && tier === 'executor-t1') opts.stepDecision = step.id
    return agent.call(packet(node, plan, checkpoint, notes), opts)
  }
  ex = await start('build:' + node + ':' + tier, null)

  for (let round = 1; ; round++) {
    const s = crw.classify(ex)
    ev.executor(tier, ex.callId, s)
    if (s === 'superseded') return { node, state: 'superseded', call: ex.callId }
    if (s === 'research') {
      // SP-EXPL-2: the question is answered and the SAME executor thread goes on; a research request is never a tier step
      const asked = await crw.explore('research', [{ id: ex.callId, question: ex.output.research_question, scope: '', files_needed: null }], { node, tier, regions: plan.regions })
      if (asked.failed) {
        await crw.hold(node, { reason: asked.failed.reason || asked.failed.status, call: asked.failed.callId, source: 'explorer' })
        return { node, state: 'held', reason: asked.failed.reason, call: asked.failed.callId }
      }
      if (asked.escalate.length > 0 || ++researched > RESEARCH_PER_TIER) {
        const esc = await crw.escalate(node, { reason: 'blocked_scope', call: ex.callId, detail: 'research_cap' })
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
    } else {
      // ready or self_failed: the run judges the worktree, never the executor's report
      head = await crw.commit(node, ex, plan.regions)
      const ci = await crw.verify(node, head)
      if (!ci.ran) {
        await crw.hold(node, { reason: 'verify_unavailable', call: ex.callId, source: 'verify' })
        return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
      }
      ev.verification(tier, head.commit, ci.pass, ci.digest)
      let verdict = null
      if (ci.pass) {
        const rv = await crw.review(node, plan, head, ci, { round, notes })
        if (rv.state !== 'verdict') return crw.reviewHalt(node, rv)
        verdict = rv.verdict
        if (verdict.verdict === 'verified') {
          const landed = await crw.land(node, { head, ci, review: rv })
          if (landed.state === 'landed') return landed
          const esc = await crw.escalate(node, { reason: landed.reason || 'land_refused', detail: landed.detail || '', ask: 'land' })
          return { node, state: 'escalated', reason: landed.reason, escalation: esc.id }
        }
        ev.review(tier, rv.call.callId, verdict.verdict, rv.findings.map((f) => f.digest))
        const found = await crw.exploreEvidence(node, verdict, { tier, regions: plan.regions })
        if (found.failed) {
          await crw.hold(node, { reason: found.failed.reason || found.failed.status, call: found.failed.callId, source: 'explorer' })
          return { node, state: 'held', reason: found.failed.reason, call: found.failed.callId }
        }
        notes = notes.concat(found.answers.filter((a) => a.answer))
      }
      if (!ev.tierStepDue(tier)) {
        // fix on the executor's own thread
        ex = await agent.call(fixPacket(node, verdict, ci, notes, ex.output), { resume: ex, schema: RECEIPT, label: 'fix:' + node + ':' + tier + ':r' + round })
        continue
      }
    }
    // SP-TIER-1 evidence stands on the tier: one step to executor-t1, and no step after it
    if (tier === 'executor-t1') {
      const esc = await crw.escalate(node, { reason: 'tiers_exhausted', evidence: ev.evidence(tier) })
      return { node, state: 'escalated', reason: 'tiers_exhausted', escalation: esc.id }
    }
    step = await crw.tierStep(node, ev.evidence(tier))
    tier = 'executor-t1'
    researched = 0
    ex = await start('build:' + node + ':' + tier, step.checkpoint)
  }
}

phase('build')
const lanes = await crw.dag(args.plan, lane, { nodes: ms.nodes, milestone: ms.id, cap: args.cap })
if (!lanes.every((l) => l && l.state === 'landed')) return { state: 'incomplete', milestone: ms.id, lanes }

phase('milestone')
const close = await crw.milestoneClose(ms.id)
const report = await crw.reportUp(ms.id, close)
return { state: 'closed', milestone: ms.id, lanes, tip: close.tip, report: report.report_id }
