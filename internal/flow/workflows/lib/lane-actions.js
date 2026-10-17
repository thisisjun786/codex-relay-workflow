// milestoneHalt takes a call of the design phase, of the close or of a re-plan that did not complete, or a gate that failed. held and
// unknown hold the milestone (a resume goes on when the cause is gone, and runs the same call again as a new turn), superseded
// stops, and everything else - refused, null_model, a gate that failed or cannot be settled, a QA that is INCONCLUSIVE, a crosscheck
// that did not verify the range - escalates.
// A call that did not complete is never read as an approval, a design, a gate, a review or an alignment. extra is what the stop of
// a close carries ({ask, evidence, fix, detail}; every member optional): the escalation asks extra.ask with extra.evidence, its detail
// adds extra.detail, and the result carries the ask, the fix packet and the crosscheck's V (extra.evidence.crosscheck) as the op
// recorded it, so the run's value hands V on unchanged.
export async function milestoneHalt(ms, source, status, reason, callId, extra) {
  const node = 'milestone:' + ms.id
  const x = extra || {}
  const cc = x.evidence && x.evidence.crosscheck ? { crosscheck: x.evidence.crosscheck } : {}
  if (status === 'held' || status === 'unknown') {
    await crw.hold(node, { reason: status === 'unknown' ? 'dispatch_unknown' : (reason || 'host_infra_unknown'), call: callId, source })
    return Object.assign({ state: 'held', source, reason, call: callId }, cc)
  }
  if (status === 'superseded') return { state: 'superseded', source, call: callId }
  const info = { reason: source + '_' + status, call: callId, detail: (reason || '') + (x.detail ? ': ' + x.detail : '') }
  if (x.ask) info.ask = x.ask
  if (x.evidence) info.evidence = x.evidence
  const esc = await crw.escalate(null, info)
  const out = { state: 'escalated', source, reason: reason || status, call: callId, escalation: esc.id }
  if (x.ask) out.ask = x.ask
  if (x.fix) out.fix = x.fix
  return Object.assign(out, cc)
}

function refusal(e) {
  const m = /refused:([a-z_0-9]+):/.exec((e && e.message) || '')
  return m ? m[1] : null
}

// land is the land of a verified head: crw.accept assembles the premerge record from the verdict and asks the relay's agent
// acceptance (dag-accept), then crw.integrate lands the accepted head (dag-integrate). A refusal of either - a finding that needs a
// disposition and is blocking by default, a reviewer the relay does not accept - is the result `land_refused` with its closed
// reason, not an exception; any other failure is thrown. o.carried are the findings the node lane carried past its last unit-check
// round trip (CRW-1465), each {id, check}: crw.accept reads each from the record of the check that carried it into the premerge
// record's defects, which the check the acceptance rests on may not name (it judged only a later fix); with none the call is the one
// it always was.
export async function land(node, o) {
  let acc
  try {
    acc = await (o.carried && o.carried.length ? crw.accept(node, o.head, { carried: o.carried }) : crw.accept(node, o.head))
  } catch (e) {
    const reason = refusal(e)
    if (!reason) throw e
    return { node, state: 'land_refused', reason, detail: e.message, step: 'accept' }
  }
  let ing
  try {
    ing = await crw.integrate(node, o.head, acc.acceptance_id)
  } catch (e) {
    const reason = refusal(e)
    if (!reason) throw e
    return { node, state: 'land_refused', reason, detail: e.message, step: 'integrate' }
  }
  return { node, state: 'landed', head: o.head.commit, acceptance: acc.acceptance_id, batch_commit: ing.batch_commit, tip: ing.tip }
}

// tierStep is the step to executor-t1 of a node: the engine mints the step decision (an id the executor-t1 call carries), and the
// host records the decision - the rule, the evidence ids and the checkpoint the new tier starts from - in flow_step_decisions.
// The step is {id, checkpoint}, the checkpoint being the head and tree of the node's last commit.
export async function tierStep(mint, node, ev) {
  const step = await mint(node, ev)
  const recorded = await crw.stepRecord(node, step, { from: ev.tier, to: 'executor-t1', letter: ev.letter, evidence: ev })
  // the id of the stored decision: the engine's id, or the one the record gave it when that id stood on another checkpoint
  return { id: recorded.id, checkpoint: recorded.checkpoint }
}
