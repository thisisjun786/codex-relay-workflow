import { install } from 'crw:lib/lane'
import { landFixPacket } from 'crw:lib/packets'
export const meta = { name: 'land-head', description: 'Land a head made outside a project-lane run through the gates of a run: the verification of its tree, a unit check of its committed range by another model family than its authors, the relay acceptance and the integration', phases: ['land'], args: { plan: 'string', milestone: 'string', issue: 'string', head: 'string', authors: 'object', fix_round: 'number' } }

// The gate-only lane (CRW-1354). The child decides how the work of an issue is made - a project-lane run, or its own subagents outside any
// run - and CRW enforces the order, the gates and the record of its landing (CRW-1316 §18.3). This workflow lands a head the child made
// outside a run: args.issue names the node of args.milestone of args.plan, args.head the commit or the branch, and args.authors the subagent
// threads that wrote it. crw.release with land releases the node to this run at the plan it read, checks the head out in a worktree of the
// run's and reads the authors' pairs back from the host; then the gates every head of a run goes through, with the rows they write:
// crw.verify (verification-record/1 of the exact tree), crw.unitCheck in the committed mode (CRW-1468: the review tool run in Go on the
// range base..head of the checkout, no reviewer call; a head made outside a run has no executor, so the row records receipt no_executor),
// crw.land (crw.accept on that check, the relay's agent acceptance, which refuses a check of the authors' model family
// independence_refused and one it cannot judge against an observed author independence_unproven, then crw.integrate). The gates are the
// only refusals: a verification that fails, a check that gives no judgment or finds a blocking defect (land_review_needs_changes, the ids
// of its findings carried on the escalation), an acceptance the relay refuses end the land with an escalation for the child, and nothing
// else stops it (an author that cannot be read back or a path outside the node's regions is a warning of the release row). No executor
// call is made: the gate-only lane fixes nothing, the child fixes outside and lands again - in a new run: a land that ends escalated gives
// its release back (endLand), and a node another run still holds is not released to this one (crw.release answers land_node_held, naming
// the run). A hold (a verifier the host cannot run now, or a check the tool gave no judgment for its quota or no answer: crw.hold with
// source unit_check) keeps the release: the run is resumed (crw flow resume), which asks the tool again, or cancelled (crw flow cancel,
// which gives the release up). Findings of a verified check that block nothing land as premerge defects.
// The fix rounds (CRW-1490): args.fix_round (0 when not given) is the round of the child's fix this land is, a non-negative integer:
// any other value (a string, a negative or fractional number, null) refuses the run at its start, fix_round_invalid, before the
// milestone is read, the node released or a fix packet made. A check that needs changes at a round under LAND_FIX_ROUNDS ends
// land_review_needs_changes with no ask - the child acts - with the round and the carried ids on the escalation and in the value, and
// the value hands the child a fix packet (landFixPacket): the findings, the range, the criteria and the land of the fix with fix_round
// one more. At LAND_FIX_ROUNDS it ends land_fix_rounds with ask land_fix_rounds, a Question for the 부모, and no fix packet.
install(crw)

const LAND_FIX_ROUNDS = 2
const fixRound = args.fix_round === undefined ? 0 : args.fix_round
if (!Number.isInteger(fixRound) || fixRound < 0) {
  return { state: 'refused', reason: 'fix_round_invalid', detail: 'args.fix_round is ' + JSON.stringify(args.fix_round) + ', not a non-negative integer (the round of the fix, 0 when not given)' }
}

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
  const ci = await crw.verify(node, head)
  if (!ci.ran) {
    await crw.hold(node, { reason: 'verify_unavailable', source: 'verify' })
    return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
  }
  if (!ci.pass) {
    return endLand(node, { reason: 'land_verification_failed', detail: ci.summary || '', evidence: { record: ci.digest, failures: ci.failures } }, { head: head.commit })
  }
  // the unit check of the committed range base..head in the land checkout: its row is what crw.accept and the relay take as the judgment
  const uc = await crw.unitCheck(node, null, { round: 1, final: true, commit: head, base: head.base })
  if (uc.hold) {
    await crw.hold(node, { reason: uc.hold, source: 'unit_check' })
    return { node, state: 'held', reason: uc.hold, source: 'unit_check', detail: uc.detail || '' }
  }
  if (uc.state !== 'ok') {
    return endLand(node, { reason: 'unit_check_' + uc.state, call: uc.check_id, detail: uc.detail || '', ask: 'configuration' }, { head: head.commit, call: uc.check_id })
  }
  const findings = uc.findings || []
  if (uc.verdict !== 'verified') {
    const carried = findings.map((f) => f.id)
    const detail = findings.map((f) => f.id + ' ' + f.file + ' ' + f.text).join('\n')
    const evidence = { carried, fix_round: fixRound }
    if (fixRound >= LAND_FIX_ROUNDS) {
      return endLand(node, { reason: 'land_fix_rounds', call: uc.check_id, detail, evidence, ask: 'land_fix_rounds' },
        { head: head.commit, call: uc.check_id, carried, fix_round: fixRound })
    }
    const fix = landFixPacket(n, findings, head, { plan: args.plan, milestone: ms.id, fix_round: fixRound + 1, authors: args.authors || [] })
    return endLand(node, { reason: 'land_review_needs_changes', call: uc.check_id, detail, evidence },
      { head: head.commit, call: uc.check_id, carried, fix_round: fixRound, fix })
  }
  const landed = await crw.land(node, { head, ci })
  if (landed.state === 'landed') return landed
  if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail, milestone: ms.id }
  // a check the relay does not take as independent of the authors is a configuration of the check, as the reviewer's was
  const ask = /^independence_/.test(landed.reason || '') ? 'configuration' : 'land'
  return endLand(node, { reason: landed.reason || 'land_refused', call: uc.check_id, detail: landed.detail || '', ask }, { head: head.commit })
}

// endLand ends a land whose head did not pass its gates: the escalation for the child, with end_land, which gives the node's release
// back (the relay closes the execution land_ended and returns its slot), so the child's next land of the node is a release of its own.
async function endLand(node, info, out) {
  const esc = await crw.escalate(node, Object.assign({}, info, { end_land: true }))
  return Object.assign({ node, state: 'escalated', reason: info.reason, escalation: esc.id }, out)
}
