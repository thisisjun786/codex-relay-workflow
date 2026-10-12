import { RECEIPT } from 'crw:lib/schemas'
import { install } from 'crw:lib/lane'
import { packet, fixPacket } from 'crw:lib/packets'
export const meta = { name: 'issue-lane', description: 'Run one oversized issue as the lane of its child: executor, the run\'s commit and verification, an independent review. The child coordinates; the executor calls make every commit', phases: ['build'], args: { node: 'string', issue: 'object' } }

// The issue lane of a child (SP-CHILD-1). An issue the architect sized for a child is released as a relay child that only coordinates:
// this script is what it runs. Its executor calls (Claude executor, then fix rounds on the same thread) author every commit, the run
// commits the executor's snapshot (crw.commit) and verifies it, and a reviewer of another model family rules on the head. The head and
// its verdict are the child's receipt: `crw flow apply` in the orchestrator's delivery turn waits for the verdict this run journals,
// checks that every commit of base..head was made by one of the executor calls recorded here, and then rules on the relay event.
//
// The plan of the issue (args.issue) is the IssuePlan the project run released the child with: the facts of the node's plan revision,
// given whole, because this run has no release of its own. This lane lands nothing - acceptance and integration are the project
// lane's, after the orchestrator applied the receipt - and it escalates with reasons that close no execution: the orchestrator decides
// what to do with the node. Every fact it uses comes from its own calls and operations, so a resume replays it through the journal.
install(crw)

const node = args.node
const plan = args.issue
// the review rounds one run gives its executor: after them the head and the last verdict are the receipt, needs_changes included
const MAX_ROUNDS = 3

// A call that did not end in a RECEIPT the run can judge: the lane holds or escalates and returns what it came to, null when the
// worktree is to be judged. A hold waits for `crw flow resume`; an escalation is the orchestrator's.
async function halted(ex, state) {
  if (state === 'superseded') return { node, state: 'superseded', call: ex.callId }
  if (state === 'scope' || state === 'research') {
    const gap = state === 'scope' ? (ex.output.gap || '') : (ex.output.research_question || '')
    const esc = await crw.escalate(node, { reason: 'child_scope_gap', call: ex.callId, detail: gap })
    return { node, state: 'escalated', reason: 'child_scope_gap', call: ex.callId, escalation: esc.id }
  }
  if (state === 'refused') {
    const esc = await crw.escalate(node, { reason: ex.reason, call: ex.callId, ask: 'configuration' })
    return { node, state: 'escalated', reason: ex.reason, call: ex.callId, escalation: esc.id }
  }
  if (state === 'null_unknown') {
    await crw.hold(node, { reason: 'dispatch_unknown', call: ex.callId })
    return { node, state: 'held', reason: 'dispatch_unknown', call: ex.callId }
  }
  if (state === 'host_held') {
    await crw.hold(node, { reason: ex.reason, call: ex.callId })
    return { node, state: 'held', reason: ex.reason, call: ex.callId }
  }
  if (state === 'null_model') {
    const esc = await crw.escalate(node, { reason: 'child_executor_null_model', call: ex.callId, detail: ex.reason || '' })
    return { node, state: 'escalated', reason: 'child_executor_null_model', call: ex.callId, escalation: esc.id }
  }
  return null
}

phase('build')
await crw.spawnDecision('EXEC_1', 'spawn', { node, role: 'executor', tier: 'executor', inputs: { label: 'build:' + node } })
let ex = await agent.call(packet(node, plan, null, []), { agentType: 'executor', node, schema: RECEIPT, label: 'build:' + node + ':executor' })
let verdict = null
let head = null
let review = null
for (let round = 1; ; round++) {
  const stop = await halted(ex, crw.classify(ex))
  if (stop) return stop
  // ready or self_failed: the run judges the worktree, never the executor's report
  head = await crw.commit(node, ex, plan.regions)
  const ci = await crw.verify(node, head)
  if (!ci.ran) {
    await crw.hold(node, { reason: 'verify_unavailable', call: ex.callId, source: 'verify' })
    return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
  }
  verdict = null
  if (ci.pass) {
    const rv = await crw.review(node, plan, head, ci, { round, notes: [] })
    if (rv.state !== 'verdict') return crw.reviewHalt(node, rv)
    verdict = rv.verdict
    review = rv.call.callId
    if (verdict.verdict === 'verified') return { node, state: 'verified', head: head.commit, review, rounds: round, base: head.parent }
  }
  if (round >= MAX_ROUNDS) break
  // fix on the executor's own thread
  ex = await agent.call(fixPacket(node, verdict, ci, [], ex.output), { resume: ex, schema: RECEIPT, label: 'fix:' + node + ':executor:r' + round })
}
return { node, state: verdict ? 'needs_changes' : 'verification_failed', head: head.commit, review, rounds: MAX_ROUNDS }
