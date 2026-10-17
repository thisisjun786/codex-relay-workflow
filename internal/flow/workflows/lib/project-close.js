import { REFLECTION } from 'crw:lib/schemas'
import { reflectionPacket } from 'crw:lib/packets'
import { reportClose } from 'crw:lib/close'

export function milestonePhase(ms, lanes, replan) {
  async function closeAndReport() {
    phase('milestone')
    // The close: the final gate over the integrated head (test evidence, and the QA receipt for a driven surface), the crosscheck over the
    // range with the passed gate as its input (two passes of a review engine in a separate tool invocation, run by the host; no agent thread id),
    // and the close recorded over the tip it judged. A gate that fails or cannot be settled ends the milestone here, and the crosscheck does
    // not run; a crosscheck that does not verify the range ends it too, and nothing is closed, reflected on or reported (crw:lib/close).
    // The halt carries what the close's stop does: the ask, the crosscheck's V in the escalation's evidence and in the run's value, and the
    // fix packet the 자식 hands on.
    const closed = await crw.closeMilestone(ms)
    if (closed.state !== 'closed') return crw.milestoneHalt(ms, closed.source, closed.status, closed.reason, closed.call, { ask: closed.ask, evidence: closed.evidence, fix: closed.fix, detail: closed.detail })
    // The architect's reflection (SP-ARCH-3): the next turn of its thread, at the integrated head the gate pinned, in a read-only checkout
    // of it. A call that did not complete is not ALIGNED: held and unknown hold the milestone, everything else escalates.
    const current = await crw.currentDesign(ms.id)
    await crw.spawnDecision('ARCH_3', 'spawn', { role: 'architect', inputs: { milestone: ms.id, revision: current.revision, head: closed.gate.head } })
    const reflected = await agent.call(reflectionPacket(ms, current, closed), Object.assign({ schema: REFLECTION, head: closed.gate.head, label: 'reflection:' + ms.id }, current.handle ? { resume: current.handle } : { agentType: 'architect' }))
    const reflection = await crw.reflection(ms.id, closed.gate, reflected)
    if (reflection.state !== 'verdict') return crw.milestoneHalt(ms, 'reflection', reflection.state, reflection.reason, reflected.callId)
    // A reason to re-plan stands whatever the alignment says: the numbers of the signals, and the alignment the reflection found.
    const due = await crw.replanSignals(ms.id, { phase: 'close', reflection: reflection.alignment })
    // The closed milestone is audited and reported before any re-plan: the acceptances are audited against the records they rest on, and
    // the report carries the audit, the gate, the crosscheck and the reflection.
    const reported = await reportClose(ms, closed, reflection.alignment)
    if (due.due) {
      // at the close every node has run: the whole DESIGN names none unstarted
      const out = await replan('close', 0, due, closed.gate.head, reflection.misaligned, [])
      if (out.state !== 'revised') return out.result
      // the correction issues are the orchestrator's: crw-add-issue and a plan revision, planned in the revision the architect just made
      const esc = await crw.escalate(null, { reason: 'replan_correction', detail: 'revision ' + out.revision + ' of milestone ' + ms.id + ' after ' + due.reasons.join(', '), ask: 'correction_issues' })
      return { state: 'replanned', milestone: ms.id, lanes, tip: closed.tip, revision: out.revision, reasons: due.reasons, escalation: esc.id, report: reported.report.report_id,
        crosscheck: closed.crosscheck.verdict, reflection: reflection.alignment, delivery: closed.delivery }
    }
    await crw.spawnDecision('ARCH_2', 'skip', { reason: 'no_replan_signal', inputs: { milestone: ms.id, revision: due.revision, plan_mismatch: due.plan_mismatch, slice_change: due.slice_change.changed, reflection: reflection.alignment } })
    return { state: 'closed', milestone: ms.id, lanes, tip: closed.tip, report: reported.report.report_id, gate: closed.gate.state, crosscheck: closed.crosscheck.verdict, reflection: reflection.alignment, delivery: closed.delivery,
      audit: { ok: reported.audit.ok, mismatch: reported.audit.mismatch, unobserved: reported.audit.unobserved } }
  }
  return closeAndReport
}
