import { install } from 'crw:lib/lane'
export const meta = { name: 'land-head', description: 'Land a head made outside a project-lane run through the gates of a run: the verification of its tree, a review by another model family than its authors, the relay acceptance and the integration', phases: ['land'], args: { plan: 'string', milestone: 'string', issue: 'string', head: 'string', authors: 'object' } }

// The gate-only lane (CRW-1354). The child decides how the work of an issue is made - a project-lane run, or its own subagents outside any
// run - and CRW enforces the order, the gates and the record of its landing (CRW-1316 §18.3). This workflow lands a head the child made
// outside a run: args.issue names the node of args.milestone of args.plan, args.head the commit or the branch, and args.authors the subagent
// threads that wrote it. crw.release with land releases the node to this run at the plan it read, checks the head out in a worktree of the
// run's and reads the authors' pairs back from the host; then the gates every head of a run goes through, with the rows they write:
// crw.verify (verification-record/1 of the exact tree), crw.review (a fresh reviewer of another model family than the authors; one of the
// same family is independence_refused), crw.land (crw.accept, the relay's agent acceptance, then crw.integrate). The gates are the only
// refusals: a verification that fails, a review that is not verified or not independent, an acceptance the relay refuses end the land with
// an escalation for the child, and nothing else stops it (an author that cannot be read back or a path outside the node's regions is a
// warning of the release row). No executor call is made: the gate-only lane fixes nothing, the child fixes outside and lands again.
install(crw)

const EPOCH_LOST = 'epoch_lost'
const epochLost = (e) => /refused:epoch_lost:/.test((e && e.message) || '')

const ms = await crw.milestone(args.plan, args.milestone)
const n = ms.nodes.find((x) => x.issue === args.issue || x.id === args.issue)
if (!n) return { state: 'refused', reason: 'issue_not_in_milestone', detail: 'milestone ' + ms.id + ' of plan ' + args.plan + ' has no node of issue ' + args.issue }

phase('land')
try {
  return await landHead(n)
} catch (e) {
  if (epochLost(e)) return { node: n.id, state: 'stopped', reason: EPOCH_LOST, detail: e.message, milestone: ms.id }
  throw e
}

async function landHead(n) {
  const node = n.id
  const rel = await crw.release(args.plan, node, ms.id, { pinned: true, land: { head: args.head || '', authors: args.authors || [] } })
  if (!rel.released) {
    return { node, state: rel.reason === EPOCH_LOST ? 'stopped' : 'unreleased', reason: rel.reason, detail: rel.detail || '', milestone: ms.id }
  }
  const head = rel.land
  // what a reviewer of the head is given: the node's plan as the relay holds it (a head made outside a run has no DESIGN)
  const plan = { issue: n.issue, title: n.title, criteria: n.criteria, criteria_digest: n.criteria_digest, regions: n.regions,
    done_condition: (n.end_condition || []).length ? n.end_condition.join('; ') : 'the criteria above are met at the head' }
  const ci = await crw.verify(node, head)
  if (!ci.ran) {
    await crw.hold(node, { reason: 'verify_unavailable', source: 'verify' })
    return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
  }
  if (!ci.pass) {
    const esc = await crw.escalate(node, { reason: 'land_verification_failed', detail: ci.summary || '', evidence: { record: ci.digest, failures: ci.failures } })
    return { node, state: 'escalated', reason: 'land_verification_failed', escalation: esc.id, head: head.commit }
  }
  const rv = await crw.review(node, plan, head, ci, { round: 1, notes: [] })
  if (rv.state !== 'verdict') return crw.reviewHalt(node, rv)
  if (rv.verdict.verdict !== 'verified') {
    const esc = await crw.escalate(node, { reason: 'land_review_needs_changes', call: rv.call.callId, detail: rv.findings.map((f) => f.ref + ' ' + f.kind).join(', ') })
    return { node, state: 'escalated', reason: 'land_review_needs_changes', escalation: esc.id, head: head.commit, call: rv.call.callId }
  }
  const landed = await crw.land(node, { head, ci, review: rv })
  if (landed.state === 'landed') return landed
  if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail, milestone: ms.id }
  const esc = await crw.escalate(node, { reason: landed.reason || 'land_refused', detail: landed.detail || '', ask: 'land' })
  return { node, state: 'escalated', reason: landed.reason, escalation: esc.id, head: head.commit }
}
