import { install } from 'crw:lib/lane'
import { landFixPacket, answerPacket } from 'crw:lib/packets'
import { RECEIPT } from 'crw:lib/schemas'
export const meta = { name: 'land-head', description: 'Land a head made outside a project-lane run through the gates of a run: the verification of its tree, a unit check of its committed range by another model family than its authors, the relay acceptance and the integration', phases: ['land'], args: { plan: 'string', milestone: 'string', issue: 'string', head: 'string', authors: 'object', fix_round: 'number' } }

// The landing lane of a head made outside a project-lane run (CRW-1354). The child decides how the work of an issue is made - a
// project-lane run, or its own subagents outside any run - and CRW enforces the order, the gates and the record of its landing
// (CRW-1316 §18.3). This workflow lands a head the child made outside a run: args.issue names the node of args.milestone of
// args.plan, args.head the commit or the branch, and args.authors the subagent threads that wrote it. crw.release with land releases
// the node to this run at the plan it read, checks the head out in a worktree of the run's and reads the authors' pairs back from
// the host; then the gates every head of a run goes through, with the rows they write: crw.verify (verification-record/1 of the
// exact tree), crw.unitCheck in the committed mode (CRW-1468: the review tool run in Go on the range base..head of the checkout, no
// reviewer call; a head made outside a run has no executor until the run's own fix call, so the first check's row records receipt
// no_executor), crw.land (crw.accept on that check, the relay's agent acceptance, which refuses a check of the authors' model family
// independence_refused and one it cannot judge against an observed author independence_unproven, then crw.integrate).
// A check that needs changes is fixed in the run (CRW-1490), on the same release and without giving the slot back: a finding that
// is only missing evidence (kind evidence_missing, with its evidence_question) is asked of an explorer (crw.explore, kind evidence)
// and the same head is checked again with the answers in the check's prompt; every other findings set goes to an executor call
// (land-fix:<node>:r<round>, landFixPacket) whose work the run commits and checks again, over the same base. The fix rounds the run
// spends are counted by args.fix_round and bounded by LAND_FIX_ROUNDS: a check that still needs changes at the cap ends
// land_fix_rounds, an escalation for the child with the last check's carried findings and no fix packet. A hold (a verifier the
// host cannot run now, a check the tool gave no judgment for, or an explorer call that did not complete) keeps the release: the run
// is resumed (crw flow resume), which asks again, or cancelled (crw flow cancel). A land that ends escalated gives its release back
// (endLand), and a node another run still holds is not released to this one (crw.release answers land_node_held, naming the run).
// Findings of a verified check that block nothing land as premerge defects.
// The fix rounds (CRW-1490): args.fix_round (0 when not given) is the round of fixes this land starts at, a non-negative integer:
// any other value (a string, a negative or fractional number, null) refuses the run at its start, fix_round_invalid, before the
// milestone is read, the node released or a fix round spent.
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
  const base = rel.land.base
  let head = rel.land
  let ci = null
  let ex = null
  let round = fixRound
  let evidence = []
  for (;;) {
    if (!ci) {
      ci = await crw.verify(node, head)
      if (!ci.ran) {
        await crw.hold(node, { reason: 'verify_unavailable', source: 'verify' })
        return { node, state: 'held', reason: 'verify_unavailable', detail: ci.error }
      }
      if (!ci.pass) {
        return endLand(node, { reason: 'land_verification_failed', detail: ci.summary || '', evidence: { record: ci.digest, failures: ci.failures } }, { head: head.commit })
      }
    }
    const uc = await crw.unitCheck(node, ex, { round: round + 1, final: round >= LAND_FIX_ROUNDS, commit: head, base: base, evidence })
    if (uc.hold) {
      await crw.hold(node, { reason: uc.hold, source: 'unit_check' })
      return { node, state: 'held', reason: uc.hold, source: 'unit_check', detail: uc.detail || '' }
    }
    if (uc.state !== 'ok') {
      return endLand(node, { reason: 'unit_check_' + uc.state, call: uc.check_id, detail: uc.detail || '', ask: 'configuration' }, { head: head.commit, call: uc.check_id })
    }
    if (uc.verdict === 'verified') {
      const landed = await crw.land(node, { head, ci })
      if (landed.state === 'landed') return landed
      if (landed.reason === EPOCH_LOST) return { node, state: 'stopped', reason: EPOCH_LOST, detail: landed.detail, milestone: ms.id }
      // a check the relay does not take as independent of the authors is a configuration of the check, as the reviewer's was
      const ask = /^independence_/.test(landed.reason || '') ? 'configuration' : 'land'
      return endLand(node, { reason: landed.reason || 'land_refused', call: uc.check_id, detail: landed.detail || '', ask }, { head: head.commit })
    }
    const findings = uc.findings || []
    const carried = findings.map((f) => f.id)
    const detail = findings.map((f) => f.id + ' ' + f.file + ' ' + f.text).join('\n')
    if (round >= LAND_FIX_ROUNDS) {
      return endLand(node,
        { reason: 'land_fix_rounds', call: uc.check_id, detail, evidence: { carried, fix_round: round }, ask: 'land_fix_rounds' },
        { head: head.commit, call: uc.check_id, carried, fix_round: round })
    }
    round++
    if (findings.length > 0 && findings.every((f) => f.kind === 'evidence_missing' && f.evidence_question && f.evidence_question.question)) {
      const questions = findings.map((f) => ({ id: f.id, question: f.evidence_question.question, scope: '', files_needed: f.evidence_question.files_needed || [] }))
      const asked = await crw.explore('evidence', questions, { node, milestone: ms.id, head: head.commit, regions: n.regions })
      if (asked.failed) {
        await crw.hold(node, { reason: asked.failed.reason || asked.failed.status, call: asked.failed.callId, source: 'explorer' })
        return { node, state: 'held', reason: asked.failed.reason || asked.failed.status, call: asked.failed.callId }
      }
      if (asked.escalate.length || asked.orchestrator.length) {
        return endLand(node, { reason: 'land_evidence_unavailable', call: uc.check_id, detail, ask: 'land', evidence: { carried, fix_round: round } }, { head: head.commit, carried, fix_round: round })
      }
      evidence = evidence.concat(asked.answers)
      continue
    }
    const label = 'land-fix:' + node + ':r' + round
    const prompt = landFixPacket(n, findings, { commit: head.commit, base }, ex && ex.output)
    if (ex) {
      ex = await agent.call(prompt, { resume: ex, schema: RECEIPT, label })
    } else {
      await crw.spawnDecision('EXEC_1', 'spawn', { node, role: 'executor', tier: 'executor', inputs: { label } })
      ex = await agent.call(prompt, { agentType: 'executor', node, schema: RECEIPT, label })
    }
    // Research is a continuation of this fix, bounded by exploreRoute's existing research cap.
    for (let research = 1; crw.classify(ex) === 'research'; research++) {
      const asked = await crw.explore('research', [{ id: ex.callId, question: ex.output.research_question, scope: '', files_needed: null }], { node, tier: 'executor', regions: n.regions })
      if (asked.failed) {
        await crw.hold(node, { reason: asked.failed.reason || asked.failed.status, call: asked.failed.callId, source: 'explorer' })
        return { node, state: 'held', reason: asked.failed.reason || asked.failed.status, call: asked.failed.callId }
      }
      if (asked.escalate.length) {
        return endLand(node, { reason: 'blocked_scope', call: ex.callId, detail: asked.escalate[0].reason }, { head: head.commit })
      }
      ex = await agent.call(answerPacket(asked.answers), { resume: ex, schema: RECEIPT, label: 'land-research:' + node + ':r' + round + ':' + research })
    }
    const state = crw.classify(ex)
    if (state === 'superseded') return { node, state, call: ex.callId }
    if (state === 'host_held' || state === 'null_unknown') {
      const reason = state === 'null_unknown' ? 'dispatch_unknown' : ex.reason
      await crw.hold(node, { reason, call: ex.callId, source: 'executor' })
      return { node, state: 'held', reason, call: ex.callId }
    }
    if (state !== 'ready' && state !== 'self_failed') {
      return endLand(node, { reason: state === 'scope' ? 'blocked_scope' : 'land_fix_' + state, call: ex.callId, detail: ex.reason || (ex.output && ex.output.gap) || '', ask: state === 'refused' ? 'configuration' : 'land' }, { head: head.commit, carried, fix_round: round })
    }
    head = await crw.commit(node, ex, n.regions)
    ci = null
    evidence = []
  }
}

// endLand ends a land whose head did not pass its gates: the escalation for the child, with end_land, which gives the node's release
// back (the relay closes the execution land_ended and returns its slot), so the child's next land of the node is a release of its own.
async function endLand(node, info, out) {
  const esc = await crw.escalate(node, Object.assign({}, info, { end_land: true }))
  return Object.assign({ node, state: 'escalated', reason: info.reason, escalation: esc.id }, out)
}
