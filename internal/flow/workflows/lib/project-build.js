import { RECEIPT } from 'crw:lib/schemas'
import { packet, answerPacket, fixPacket, subTaskPacket } from 'crw:lib/packets'

// A fenced write after another session claimed the plan's coordinator epoch is refused epoch_lost (CRW-1285): the lane ends at that
// write with the reason, and neither escalates nor writes anything more at the relay; the run ends stopped on it (below).
export const EPOCH_LOST = 'epoch_lost'
export const epochLost = (e) => /refused:epoch_lost:/.test((e && e.message) || '')
export async function lane(n, rel, sig) {
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
  // the sub-tasks of a node sized for a split run in order on one executor thread. Each gets a scoped unit check before its
  // commit. Earlier commits are checkpoints; only after the last does the run verify and check the node's whole base..head
  // range, and acceptance rests on that committed check alone. done counts committed sub-tasks; failed keeps self_failed
  // receipts as context. A rebuild or tier step starts from the last checkpoint and the first sub-task not yet committed.
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
      // ready or self_failed of a split node: the run judges the worktree, never the executor's report. Every sub-task's own
      // change is checked, scoped to it, before its checkpoint commit — the last one too; work after all of them are
      // committed (a whole-node review fix, a verification fix, a tier rebuild) gets only the whole-range committed check.
      const currentSub = subs[split.done]
      const scoped = currentSub
        ? await judge(await crw.unitCheck(node, ex, { round, final: fixes >= UNIT_CHECK_ROUNDS, sub: currentSub.id }), round)
        : {}
      if (scoped.end) return scoped.end
      if (scoped.fix) {
        ex = await scoped.fix()
        continue
      }
      if (!scoped.step) {
        if (split.done + 1 < subs.length) {
          // the scoped check passed: checkpoint this sub-task in its own regions and continue on the same thread.
          // A self_failed receipt is context for the next sub-task; the review determines whether the work passes.
          const sub = subs[split.done]
          await crw.commit(node, ex, sub.edit_regions)
          if (s === 'self_failed') split.failed[sub.id] = { summary: (ex.output && ex.output.summary) || '', gap: (ex.output && ex.output.gap) || '' }
          split.done++
          ex = await agent.call(subTaskPacket(plan, split.done, split.failed), { resume: ex, schema: RECEIPT, label: 'subtask:' + node + ':' + tier + ':s' + (split.done + 1) })
          continue
        }
        // commit the last checked sub-task, or a whole-node fix after all sub-tasks; verify and review the whole range below
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
      }
      // a scoped step reaches the tier-step code below without committing or starting the next sub-task
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
