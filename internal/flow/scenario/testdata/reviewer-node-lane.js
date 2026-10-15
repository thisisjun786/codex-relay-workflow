// The node lane of the old reviewer path: the node lane of the project lane before CRW-1465 (dfc9d675), kept as test data. Every
// executor head is committed and verified, a fresh reviewer thread judges it (crw.review), crw.reviewHalt takes a review that is no
// verdict, crw.exploreEvidence answers an evidence question before the fix, and the fix goes on the executor's own thread. The project
// lane calls none of that any more; crw:lib/lane and the host keep crw.review, crw.reviewHalt and crw.exploreEvidence for the journals
// of old runs until they go (Z-1). The scenarios of that path (reviewer_path_test.go) run the project lane with this text in place of
// its own node lane (reviewerLane), so the kept path still runs against today's operations and helpers.
const RESEARCH_PER_TIER = 2

// The lane of one issue node. It reads no variable of the script that another lane may have changed: its facts are the plan of
// its release revision and the results of its own calls.
async function nodeLane(n, rel, sig) {
  const node = n.id
  const plan = await crw.issuePlan(node)
  if (plan.size_fit === 'child') {
    // the issue-child path is retired (CRW-1415): a node sized for a child goes up as a size split, and no child is started
    const esc = await crw.escalate(node, { reason: 'size_split', detail: 'child_retired', evidence: { size_reason: plan.size_reason } })
    return { node, state: 'escalated', reason: 'size_split', escalation: esc.id }
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
    // the RECEIPT of the call is a trigger of the signal path
    sig.receipt(ex)
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
          if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail }
          const esc = await crw.escalate(node, { reason: landed.reason || 'land_refused', detail: landed.detail || '', ask: 'land' })
          return { node, state: 'escalated', reason: landed.reason, escalation: esc.id }
        }
        ev.review(tier, rv.call.callId, verdict.verdict, rv.findings.map((f) => f.digest))
        const found = await crw.exploreEvidence(node, verdict, { tier, regions: plan.regions, head: head.commit })
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
      await crw.spawnDecision('TIER_2', 'refused', { node, role: tier, tier, reason: 'tiers_exhausted', inputs: ev.evidence(tier) })
      const esc = await crw.escalate(node, { reason: 'tiers_exhausted', evidence: ev.evidence(tier) })
      return { node, state: 'escalated', reason: 'tiers_exhausted', escalation: esc.id }
    }
    step = await crw.tierStep(node, ev.evidence(tier))
    tier = 'executor-t1'
    researched = 0
    ex = await start('build:' + node + ':' + tier, step.checkpoint)
  }
}

