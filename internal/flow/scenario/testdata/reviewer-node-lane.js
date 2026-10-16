// The node lane of the old reviewer path: the node lane of the project lane before CRW-1465 (dfc9d675), kept as test data with the
// composites of crw:lib/lane of the build before CRW-1447 that it called. Every executor head is committed and verified, a fresh
// reviewer thread judges it (review: the reviewer call and its crw.review record), reviewHalt takes a review that is no verdict,
// exploreEvidence answers an evidence question before the fix, and the fix goes on the executor's own thread. The project lane calls
// none of that, and since CRW-1447 neither crw:lib/lane nor the lane has any of it: the scenarios of that path (reviewer_path_test.go)
// and the legacy refusal of an old run (legacy_reviewer_test.go) run the project lane with this text in place of its own node lane and
// with the old build's crw.review registered (reviewerLane), so an old run's journal is written as that build wrote it.
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
        const rv = await review(node, plan, head, ci, { round, notes })
        if (rv.state !== 'verdict') return reviewHalt(node, rv)
        verdict = rv.verdict
        if (verdict.verdict === 'verified') {
          const landed = await crw.land(node, { head, ci, review: rv })
          if (landed.state === 'landed') return landed
          if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail }
          const esc = await crw.escalate(node, { reason: landed.reason || 'land_refused', detail: landed.detail || '', ask: 'land' })
          return { node, state: 'escalated', reason: landed.reason, escalation: esc.id }
        }
        ev.review(tier, rv.call.callId, verdict.verdict, rv.findings.map((f) => f.digest))
        const found = await exploreEvidence(node, verdict, { tier, regions: plan.regions, head: head.commit })
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

// ---- the composites of the old reviewer path (crw:lib/lane and crw:lib/packets before CRW-1447) ----

// exploreEvidence is SP-EXPL-3: a verdict with a defect of kind evidence_missing sends its evidence_question to an explorer
// before the node's next round; its FINDINGS go to the next fix packet and the next reviewer packet. A verdict with no such defect
// records the skip. ctx.head is the committed head the reviewer judged: the explorer's read-only worktree is made at it.
async function exploreEvidence(node, verdict, ctx) {
  const questions = verdict.defects
    .filter((d) => d.kind === 'evidence_missing' && d.evidence_question)
    .map((d) => ({ id: d.ref, question: d.evidence_question.question, scope: d.file, files_needed: d.evidence_question.files_needed }))
  return crw.explore('evidence', questions, Object.assign({ node }, ctx || {}))
}

// review is the reviewer call of a round and its record: a fresh reviewer thread reads the committed head (SP-REV-1) and answers
// VERDICT, and crw.review's record decides the state of the answer. A reviewer whose answer is null_model is asked once more on a
// fresh thread; the result of that second call is the result.
async function review(node, plan, head, ci, opts) {
  const round = opts.round || 1
  const packet = reviewPacket(node, plan, head, ci, opts)
  await crw.spawnDecision('REV_1', 'spawn', { node, role: 'reviewer', inputs: { round, head: head.commit } })
  let call = await agent.call(packet, { agentType: 'reviewer', node, head: head.commit, schema: VERDICT, label: 'review:' + node + ':r' + round })
  let rv = await crw.review(node, head, plan.criteria_digest, call, {})
  if (rv.state === 'null_model') {
    call = await agent.call(packet, { agentType: 'reviewer', node, head: head.commit, schema: VERDICT, label: 'review:' + node + ':r' + round + ':retry' })
    rv = await crw.review(node, head, plan.criteria_digest, call, { retried: true })
  }
  return rv
}

// reviewHalt takes the review results that are not a verdict, which are never an executor fix and never tier evidence: held and
// unknown hold the node, refused (independence_refused included) escalates as a configuration fault, null_model - asked twice
// already - escalates, superseded stops.
async function reviewHalt(node, rv) {
  const call = rv.call.callId
  switch (rv.state) {
    case 'held':
      await crw.hold(node, { reason: rv.reason || 'host_infra_unknown', call, source: 'review' })
      return { node, state: 'held', reason: rv.reason, call }
    case 'unknown':
      await crw.hold(node, { reason: 'dispatch_unknown', call, source: 'review' })
      return { node, state: 'held', reason: 'dispatch_unknown', call }
    case 'refused': {
      const esc = await crw.escalate(node, { reason: rv.reason || 'reviewer_refused', call, ask: 'configuration' })
      return { node, state: 'escalated', reason: rv.reason || 'reviewer_refused', call, escalation: esc.id }
    }
    case 'null_model': {
      const esc = await crw.escalate(node, { reason: 'reviewer_null_model', call, detail: rv.reason || '' })
      return { node, state: 'escalated', reason: 'reviewer_null_model', call, escalation: esc.id }
    }
    default:
      return { node, state: 'superseded', call }
  }
}

function reviewList(items, empty) {
  if (!items || items.length === 0) return ['- ' + (empty || 'none')]
  return items.map((s) => '- ' + s)
}

function reviewSection(title, lines) {
  return [title + ':'].concat(lines).join('\n')
}

// reviewPacket is the packet of a node reviewer's call.
function reviewPacket(node, plan, head, ci, opts) {
  const parts = [
    'ROLE: reviewer. Review the head ' + head.commit + ' of node ' + node + ' (' + plan.issue + ') against its criteria; round ' + (opts.round || 1) + '.',
    // the two lines `crw flow audit` reads back from the thread on the host: what the acceptance of this review stands on
    'HEAD: ' + head.commit + '\nCRITERIA DIGEST: ' + plan.criteria_digest,
    reviewSection('CRITERIA', reviewList((plan.criteria || []).map((c, i) => 'c' + (i + 1) + ': ' + c))),
    reviewSection('DONE CONDITION', [plan.done_condition]),
    reviewSection('EDIT REGIONS', reviewList(plan.regions, 'undeclared')),
    reviewSection('CHANGED PATHS', reviewList(head.changed)),
    reviewSection('OUT OF SCOPE', reviewList(head.out_of_scope, 'none')),
    'VERIFICATION: ' + (ci.pass ? 'passed' : 'failed') + ' (' + ci.command + ') record ' + ci.digest,
  ]
  const notes = opts.notes || []
  if (notes.length) parts.push(reviewSection('FINDINGS OF THE EXPLORER (use them; do not ask again)', notes.map((n) => '- ' + n.question + '\n  ' + n.answer)))
  parts.push('Answer with VERDICT. verified means every criterion is met and no defect blocks. Answer one entry of criteria for each criterion c1, c2, ... above, each once, under its id. A defect you cannot judge from the head is of kind evidence_missing and carries an evidence_question. The impact of each defect is one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable: a defect of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block. A verdict of verified over a defect that blocks is refused as no verdict.')
  return parts.join('\n\n')
}
