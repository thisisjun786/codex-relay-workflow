// crw:lib/close: the close of a milestone of the project lane (CRW-1206). A milestone closes over evidence the run made itself, in
// this order, and the order is what the close guarantees:
//
//   the final gate    crw.finalGate pins the integrated head H and judges the evidence that stands on it (test evidence, and the QA
//                     receipt when a criterion of the milestone has a driven surface). Evidence that is missing or stale is made at
//                     H - crw.verify, and the QA calls - and the gate is judged again. A gate that fails or cannot be settled stops
//                     the close: the crosscheck does not run.
//   the crosscheck    one round of crw.crosscheck over the range of the passed gate (CRW-1473): two passes (A coverage, B bug hunt) of a
//                     review engine of another model family than every author of the milestone, run by the host in Go, their findings
//                     merged into one verdict V by one rule (ops.CrosscheckRule, premerge.Blocks). Only a V of verified (accepts) lets
//                     the milestone close. A V of needs_changes stops the close as crosscheck_pending with V and the fix packet
//                     (crosscheckFixPacket), which the 자식 hands to the 부모 (ask crosscheck_pending); a crosscheck that held holds
//                     the milestone, one that was refused asks for the configuration, and one that failed or gave no verdict asks with
//                     its own state (crosscheck_failed, crosscheck_no_verdict).
//   the close         crw.milestoneClose records the tip it judged over {gate, crosscheck: V}, and refuses a close whose V is not a
//                     verified one of the gate's head and milestone (close_unverified); a target that moved on after the gate is
//                     judged again from its new head.
//
// Then reportClose audits every acceptance of the milestone against the record it rests on (auditAcceptances: a unit check's row, or an
// old review's thread on the host), and reports the close up with the audit, its gate, its crosscheck V and the reflection of the
// architect. The close archives no thread (CRW-1447). A composite takes no position of its own; every position it uses is that of a
// call or of an operation it makes, so a resume replays it through the journal like any other code of the script. It reads no variable
// outside its arguments.
//
// A composite does not stop the milestone itself. It answers {state: 'stop', source, status, reason, call} (with ask, evidence and
// fix when the stop has them), and the script halts the milestone as it does for any other call that did not complete.

import { QA_CAPTURE, QA_PASS, QA_RECEIPT } from 'crw:lib/schemas'
import { qaCapturePacket, qaPassPacket, qaSynthesisPacket, qaSelfReviewPacket, crosscheckPacket, crosscheckFixPacket } from 'crw:lib/packets'

// The rounds the gate makes evidence in before it gives up: the head moves while the evidence is made, and each round judges the
// head it finds. The attempts the close makes before it gives up on a target that keeps moving.
export const GATE_ROUNDS = 3
export const CLOSE_ATTEMPTS = 3

const PASSES = ['A', 'B']

// The two passes of a crosscheck round: A coverage, B bug hunt (ops.CrosscheckPassA, ops.CrosscheckPassB).
const CROSSCHECK_PASSES = ['A', 'B']

function stop(source, status, reason, call, extra) {
  return Object.assign({ state: 'stop', source, status, reason: reason || status, call: call || null }, extra || {})
}

// qa makes the QA evidence of the gate at its head: the QA capture call (SP-QA-1), then either its self-review (C2) or the two
// oracle passes (SP-QA-2) and the synthesis on the thread of the capture call, and the receipt that records them. The run starts
// every call: the capture call is bound to spawn nothing, and the passes are explorer calls of the run.
async function qa(ms, g) {
  await crw.spawnDecision('QA_1', 'spawn', { role: 'executor', inputs: { milestone: ms.id, head: g.head, grade: g.grade } })
  const cap = await agent.call(qaCapturePacket(ms, g), { agentType: 'executor', qa: true, head: g.head, schema: QA_CAPTURE, label: 'qa:' + ms.id })
  let passes = []
  let syn = null
  // The capture's artifacts are files of its own scratch worktree and every later call reads a checkout of its own: crw.qaCapture
  // hands on the captures with their paths resolved against the directory the capture ran in, and says when the capture cannot be
  // handed on (not a capture, a path that cannot be resolved, a tree that is not the pinned head). The passes and the synthesis are
  // asked only for a capture that can; the receipt gives the reason otherwise.
  const view = cap.status === 'completed' ? await crw.qaCapture(ms.id, g, cap) : null
  if (view && view.ok) {
    if (g.qa_passes) {
      for (const p of PASSES) await crw.spawnDecision('QA_2', 'spawn', { role: 'explorer', inputs: { milestone: ms.id, head: g.head, pass: p } })
      const ran = await parallel(PASSES.map((p) => () => agent.call(qaPassPacket(p, ms, g, view), {
        agentType: 'explorer', head: g.head, schema: QA_PASS, label: 'qa-pass:' + p + ':' + ms.id,
      })), { keys: PASSES, label: 'qa-passes' })
      passes = ran.map((c) => c || { callId: '', status: 'failed', reason: 'pass_thrown', meta: { role: 'explorer' } })
      // a pass that did not complete makes the QA INCONCLUSIVE: the synthesis is not asked for, and no verdict is read
      if (passes.every((c) => c.status === 'completed')) {
        syn = await agent.call(qaSynthesisPacket(ms, g, passes.map((c) => c.output)), { resume: cap, head: g.head, schema: QA_RECEIPT, label: 'qa-synthesis:' + ms.id })
      }
    } else {
      syn = await agent.call(qaSelfReviewPacket(ms, g, view), { resume: cap, head: g.head, schema: QA_RECEIPT, label: 'qa-review:' + ms.id })
    }
  }
  return crw.qaReceipt(ms.id, g, cap, passes, syn)
}

// closeGate judges the evidence of the integrated head and makes what is missing, until the gate passes, fails, or cannot be
// settled.
async function closeGate(ms) {
  let g = null
  for (let round = 1; round <= GATE_ROUNDS; round++) {
    g = await crw.finalGate(ms.id)
    if (g.state !== 'pending') return g
    for (const command of g.need_tests) {
      const v = await crw.verify('', g.head, { milestone: ms.id, command, base: g.base })
      // a verification that could not run is not a failed one: the milestone waits for the verifier
      if (!v.ran) return stop('final_gate', 'held', 'verify_unavailable', null, { detail: v.error, gate: g })
    }
    if (g.need_qa) await qa(ms, g)
  }
  return g
}

// gateStop is the stop of a gate that did not pass.
function gateStop(g) {
  if (g.state === 'pending') return stop('final_gate', 'unsettled', 'gate_unsettled', null, { gate: g })
  if (g.reason === 'test_failed') return stop('final_gate', 'test_failed', 'test_failed', null, { gate: g })
  return stop('qa', g.reason === 'qa_inconclusive' ? 'inconclusive' : 'failed', g.reason, null, { gate: g })
}

// accepts says whether the crosscheck X lets the close go on: a verdict, and one whose word is verified (X.verdict is V, its word at
// .verdict).
function accepts(x) {
  return x.state === 'verdict' && !!x.verdict && x.verdict.verdict === 'verified'
}

// crosscheckRound is one round of the crosscheck over the passed gate: the text of each pass (crosscheckPacket), then crw.crosscheck,
// which runs both passes in Go and answers X = {state, reason, hold, detail, verdict: V, questions}. This revision runs round 1 only.
export async function crosscheckRound(ms, g, opts) {
  const o = opts || {}
  const prompts = {}
  for (const p of CROSSCHECK_PASSES) prompts[p] = crosscheckPacket(ms, g, p, { prior: o.prior, evidence: o.evidence })
  return crw.crosscheck(ms.id, g, { round: 1, evidence_round: o.evidence_round || 0, prompts })
}

// crosscheckStop is the stop of a crosscheck that did not verify the range. A verdict of needs_changes keeps the milestone open as
// crosscheck_pending: V goes into the escalation's evidence and the fix packet with it, and the 자식 hands both to the 부모 (ask
// crosscheck_pending). A crosscheck that held holds the milestone (a resume runs the round again); one that was refused (an author
// pair that is undecided, no engine of another family) asks for the configuration; one that failed or gave no verdict asks with
// its state.
function crosscheckStop(ms, x) {
  const detail = x.detail || ''
  switch (x.state) {
    case 'verdict':
    case 'evidence':
      return stop('crosscheck', 'pending', 'crosscheck_pending', null, { ask: 'crosscheck_pending', evidence: { crosscheck: x.verdict }, fix: crosscheckFixPacket(ms, x.verdict), detail })
    case 'held':
      return stop('crosscheck', 'held', x.hold || x.reason, null, { detail })
    case 'refused':
      return stop('crosscheck', 'refused', x.reason, null, { ask: 'configuration', detail })
    case 'no_verdict':
      return stop('crosscheck', 'no_verdict', x.reason, null, { ask: 'crosscheck_no_verdict', detail })
    default:
      return stop('crosscheck', 'failed', x.reason || 'crosscheck_' + x.state, null, { ask: 'crosscheck_failed', detail })
  }
}

export async function closeMilestone(ms, opts) {
  for (let attempt = 1; attempt <= CLOSE_ATTEMPTS; attempt++) {
    const g = await closeGate(ms)
    if (g.state === 'stop') return g
    if (g.state !== 'pass') return gateStop(g)
    const x = await crosscheckRound(ms, g, opts)
    // a crosscheck that does not verify the range keeps the milestone open; one whose passes read no repository (CRW-1504: both void, or
    // the one void pass void again on its rerun) is no_verdict, which stops the close with ask crosscheck_no_verdict for the 부모
    if (!accepts(x)) return crosscheckStop(ms, x)
    const close = await crw.milestoneClose(ms.id, { gate: g, crosscheck: x.verdict })
    if (!close.moved) return { state: 'closed', tip: close.tip, close, gate: g, crosscheck: x.verdict }
  }
  return stop('final_gate', 'unsettled', 'close_head_moved', null, {})
}

// auditAcceptances audits the acceptances of the milestone against the records they rest on (crw.audit). A mismatch is a fault of the
// record and goes to the orchestrator; an acceptance that could not be observed is not a pass. Nothing is archived.
export async function auditAcceptances(ms) {
  const audited = await crw.audit(ms.id)
  if (audited.mismatch > 0) {
    const bad = audited.results.filter((r) => r.state === 'mismatch').map((r) => r.node + ': ' + r.reason)
    await crw.escalate(null, { reason: 'audit_mismatch', detail: bad.join(' | ') })
  }
  return { audit: audited }
}

// reportClose is the end of a closed milestone, after the reflection and the re-plan signals and before any re-plan: auditAcceptances,
// then the report up with the audit, the close's gate, its crosscheck V and the reflection's alignment (ALIGNED, MISALIGNED or
// NOT_ASSESSED from the project lane). A milestone that is re-planned is audited and reported first.
export async function reportClose(ms, closed, reflection) {
  const audited = await auditAcceptances(ms)
  const report = await crw.reportUp(ms.id, Object.assign({}, closed.close, { audit: audited.audit, crosscheck: closed.crosscheck, reflection }))
  return { audit: audited.audit, report }
}
