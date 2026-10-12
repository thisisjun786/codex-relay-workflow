// crw:lib/close: the close of a milestone of the project lane (CRW-1206). A milestone closes over evidence the run made itself, in
// this order, and the order is what the close guarantees:
//
//   the final gate    crw.finalGate pins the integrated head H and judges the evidence that stands on it (test evidence, and the QA
//                     receipt when a criterion of the milestone has a driven surface). Evidence that is missing or stale is made at
//                     H - crw.verify, and the QA calls - and the gate is judged again. A gate that fails or cannot be settled stops
//                     the close: reviewer-final does not run.
//   reviewer-final    one reviewer-final call over the range, with the passed gate as its input.
//   the close         crw.milestoneClose records the tip it judged; a target that moved on after the gate is judged again from
//                     its new head.
//
// Then settleReviews audits every acceptance of the milestone against the reviewer threads on the host and archives the threads
// whose audit passed. A composite takes no position of its own; every position it uses is that of a call or of an operation it
// makes, so a resume replays it through the journal like any other code of the script. It reads no variable outside its arguments.
//
// A composite does not stop the milestone itself. It answers {state: 'stop', source, status, reason, call}, and the script halts
// the milestone as it does for any other call that did not complete.

import { QA_CAPTURE, QA_PASS, QA_RECEIPT, VERDICT_FINAL } from 'crw:lib/schemas'
import { qaCapturePacket, qaPassPacket, qaSynthesisPacket, qaSelfReviewPacket, finalPacket } from 'crw:lib/packets'

// The rounds the gate makes evidence in before it gives up: the head moves while the evidence is made, and each round judges the
// head it finds. The attempts the close makes before it gives up on a target that keeps moving.
export const GATE_ROUNDS = 3
export const CLOSE_ATTEMPTS = 3

const PASSES = ['A', 'B']

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

// gate judges the evidence of the integrated head and makes what is missing, until the gate passes, fails, or cannot be settled.
async function gate(ms) {
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

// finalReview is the reviewer-final call over the range of the passed gate. A reviewer whose answer is null_model is asked once
// more on a fresh thread, as a node's reviewer is.
async function finalReview(ms, g) {
  await crw.spawnDecision('REVF_1', 'spawn', { role: 'reviewer-final', inputs: { milestone: ms.id, head: g.head, gate: g.state } })
  const packet = finalPacket(ms, g)
  let call = await agent.call(packet, { agentType: 'reviewer-final', head: g.head, schema: VERDICT_FINAL, label: 'reviewer-final:' + ms.id })
  let rv = await crw.reviewerFinal(ms.id, g, call, {})
  if (rv.state === 'null_model') {
    call = await agent.call(packet, { agentType: 'reviewer-final', head: g.head, schema: VERDICT_FINAL, label: 'reviewer-final:' + ms.id + ':retry' })
    rv = await crw.reviewerFinal(ms.id, g, call, { retried: true })
  }
  return rv
}

export async function closeMilestone(ms) {
  for (let attempt = 1; attempt <= CLOSE_ATTEMPTS; attempt++) {
    const g = await gate(ms)
    if (g.state === 'stop') return g
    if (g.state !== 'pass') return gateStop(g)
    const f = await finalReview(ms, g)
    if (f.state !== 'verdict') return stop('reviewer_final', f.state, f.reason, f.call.callId)
    const close = await crw.milestoneClose(ms.id, { gate: g, final: f })
    if (!close.moved) return { state: 'closed', tip: close.tip, close, gate: g, final: f }
  }
  return stop('final_gate', 'unsettled', 'close_head_moved', null, {})
}

// settleReviews audits the acceptances of the milestone against the reviewer threads on the host, and archives the threads whose
// audit passed. A mismatch is a fault of the record and goes to the orchestrator; an acceptance that could not be observed keeps
// its thread and is not a pass.
export async function settleReviews(ms) {
  const audited = await crw.audit(ms.id)
  if (audited.mismatch > 0) {
    const bad = audited.results.filter((r) => r.state === 'mismatch').map((r) => r.node + ': ' + r.reason)
    await crw.escalate(null, { reason: 'audit_mismatch', detail: bad.join(' | ') })
  }
  const archived = await crw.archive(ms.id)
  return { audit: audited, archive: archived }
}
