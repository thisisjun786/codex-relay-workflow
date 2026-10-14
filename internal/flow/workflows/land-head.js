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
// warning of the release row). No executor call is made: the gate-only lane fixes nothing, the child fixes outside and lands again - in a
// new run: a land that ends escalated gives its release back (endLand), and a node another run still holds is not released to this one
// (crw.release answers land_node_held, naming the run). A hold (a verifier or a reviewer the host cannot run now) keeps the release: the
// run is resumed (crw flow resume) or cancelled (crw flow cancel, which gives the release up).
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
    return endLand(node, { reason: 'land_verification_failed', detail: ci.summary || '', evidence: { record: ci.digest, failures: ci.failures } }, { head: head.commit })
  }
  const rv = await crw.review(node, plan, head, ci, { round: 1, notes: [] })
  if (rv.state === 'held' || rv.state === 'unknown') return crw.reviewHalt(node, rv)
  if (rv.state === 'refused') return endLand(node, { reason: rv.reason || 'reviewer_refused', call: rv.call.callId, ask: 'configuration' }, { call: rv.call.callId })
  if (rv.state === 'null_model') return endLand(node, { reason: 'reviewer_null_model', call: rv.call.callId, detail: rv.reason || '' }, { call: rv.call.callId })
  if (rv.state !== 'verdict') return endLand(node, { reason: 'land_review_superseded', call: rv.call.callId }, { call: rv.call.callId })
  if (rv.verdict.verdict !== 'verified') {
    return endLand(node, { reason: 'land_review_needs_changes', call: rv.call.callId, detail: rv.findings.map((f) => f.ref + ' ' + f.kind).join(', ') }, { head: head.commit, call: rv.call.callId })
  }
  const landed = await crw.land(node, { head, ci, review: rv })
  if (landed.state === 'landed') return landed
  if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail, milestone: ms.id }
  return endLand(node, { reason: landed.reason || 'land_refused', detail: landed.detail || '', ask: 'land' }, { head: head.commit })
}

// endLand ends a land whose head did not pass its gates: the escalation for the child, with end_land, which gives the node's release
// back (the relay closes the execution land_ended and returns its slot), so the child's next land of the node is a release of its own.
async function endLand(node, info, out) {
  const esc = await crw.escalate(node, Object.assign({}, info, { end_land: true }))
  return Object.assign({ node, state: 'escalated', reason: info.reason, escalation: esc.id }, out)
}
