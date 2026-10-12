// crw:lib/lane: the composites of the project lane. Each is a sequence of agent calls and crw operations that the script would
// otherwise repeat at every site, written once so that the sequence is the same everywhere: crw.dag (the lane scheduler),
// crw.review (the reviewer call and its record), crw.reviewHalt, crw.explore and crw.exploreEvidence (the explorer fan-out and its
// record), crw.land (accept, then integrate) and crw.evidence (the tier evidence of a node).
//
// install(crw) makes them members of the crw object, next to the operations the host registers (internal/flow/ops): the script
// then reads as the design writes it, `crw.review(...)`, `crw.land(...)`. A composite is not an operation of its own: it takes no
// position of its path, and every position it uses is that of a call or of an operation it makes, so a resume replays it through
// the journal like any other code of the script. It reads no variable outside its arguments.

import { FINDINGS, VERDICT } from 'crw:lib/schemas'
import { evidence } from 'crw:lib/evidence'
import { explorerPacket, reviewPacket } from 'crw:lib/packets'

// The executors a project runs at once (docs: at most three, and never more than the region-disjoint ready nodes).
export const DEFAULT_CAP = 3
// The explorers an SP-EXPL-1 fan-out runs at once.
export const EXPLORER_CONCURRENCY = 3

function chunks(list, n) {
  const out = []
  for (let i = 0; i < list.length; i += n) out.push(list.slice(i, i + n))
  return out
}

// regionsOverlap says whether two nodes may edit the same place: an undeclared region overlaps every other, a directory (ending
// in /) overlaps what is under it, and two equal paths overlap.
export function regionsOverlap(a, b) {
  if (!a || !b || a.length === 0 || b.length === 0) return true
  for (const x of a) {
    for (const y of b) {
      if (x === y) return true
      if (x.endsWith('/') && y.startsWith(x)) return true
      if (y.endsWith('/') && x.startsWith(y)) return true
    }
  }
  return false
}

// admissionRegions are the regions the lane admits a node by: the plan's declared regions together with the edit regions of the
// approved DESIGN. The executor of a node is scoped to the DESIGN's edit_regions and the scheduler's own gate reads the plan's, so
// two nodes run at once only when neither pair overlaps. A node with no regions on either side is undeclared, and an undeclared node
// overlaps every other.
export function admissionRegions(planRegions, editRegions) {
  if (!planRegions || planRegions.length === 0 || !editRegions || editRegions.length === 0) return []
  const out = planRegions.slice()
  for (const r of editRegions) if (!out.includes(r)) out.push(r)
  return out
}

// dag runs the lane function of every node of a milestone: in the order the scheduler offers the nodes (the order of
// opts.nodes), a node starting as soon as the nodes it waits for have landed, no more than opts.cap nodes at once, and never two
// nodes whose edit regions overlap. There is no barrier: a node starts when it can, not when a wave ends. Each node runs on a
// path of its own (its id), starts with crw.release - the scheduler's admission and the slot - and ends with the lane's result.
// A node whose predecessor did not land is not started: its result is blocked. The result is the list of the lanes' results in
// the order of opts.nodes.
export async function dag(planId, lane, opts) {
  const nodes = opts.nodes
  const cap = opts.cap > 0 ? opts.cap : DEFAULT_CAP
  const byId = {}
  const state = {}
  const gates = {}
  const blockedBy = {}
  for (const n of nodes) {
    byId[n.id] = n
    state[n.id] = 'pending'
    const gate = {}
    gate.promise = new Promise((resolve) => { gate.open = resolve })
    gates[n.id] = gate
  }
  // a node waits for nodes that wait for it would never start: the plan is a DAG, and a script that is given another shape says so
  const visiting = {}
  const visit = (id, trail) => {
    if (visiting[id] === 'done') return
    if (visiting[id] === 'open') throw new TypeError('crw.dag: the nodes have a cycle through ' + trail.concat(id).join(' -> '))
    visiting[id] = 'open'
    for (const d of byId[id].after || []) if (byId[d]) visit(d, trail.concat(id))
    visiting[id] = 'done'
  }
  for (const n of nodes) visit(n.id, [])
  const active = () => nodes.filter((n) => state[n.id] === 'running')
  function pump() {
    let again = true
    while (again) {
      again = false
      for (const n of nodes) {
        if (state[n.id] !== 'pending') continue
        const deps = (n.after || []).filter((d) => byId[d])
        if (deps.some((d) => state[d] === 'pending' || state[d] === 'running')) continue
        const failed = deps.filter((d) => state[d] !== 'landed')
        if (failed.length > 0) {
          state[n.id] = 'ended'
          blockedBy[n.id] = failed
          gates[n.id].open(false)
          again = true
          continue
        }
        const running = active()
        if (running.length >= cap) continue
        if (running.some((r) => regionsOverlap(r.regions, n.regions))) continue
        state[n.id] = 'running'
        gates[n.id].open(true)
      }
    }
  }
  const thunks = nodes.map((n) => async () => {
    const go = await gates[n.id].promise
    if (!go) return { node: n.id, state: 'blocked', blocked_by: blockedBy[n.id] }
    let out
    try {
      const rel = await crw.release(planId, n.id, opts.milestone)
      if (!rel.released && rel.kind === 'executor') out = { node: n.id, state: 'unreleased', reason: rel.reason }
      else out = await lane(n, rel)
    } catch (e) {
      state[n.id] = 'ended'
      pump()
      throw e
    }
    state[n.id] = out && out.state === 'landed' ? 'landed' : 'ended'
    pump()
    return out
  })
  pump()
  return parallel(thunks, { keys: nodes.map((n) => n.id), label: 'dag' })
}

// explore routes the questions of an asker (crw.exploreRoute decides: reuse, inline, spawn, hand to the orchestrator, skip),
// runs the explorers it spawned at most EXPLORER_CONCURRENCY at a time, and records the answers (crw.exploreRecord), so a
// question of the same digest asked again in the run reuses the answer. The result lists an answer for each question, the
// questions that went to the orchestrator thread or that exhausted the asker's research, and the call that failed when an
// explorer did not complete.
export async function explore(kind, questions, ctx) {
  const plan = await crw.exploreRoute(kind, ctx, questions)
  const spawn = plan.routes.filter((r) => r.route === 'spawn')
  const found = {}
  let failed = null
  for (const wave of chunks(spawn, EXPLORER_CONCURRENCY)) {
    const calls = await parallel(wave.map((r) => () => agent.call(explorerPacket(kind, r, ctx), {
      agentType: 'explorer', node: ctx.node || undefined, head: ctx.head || undefined, schema: FINDINGS, label: 'explore:' + (ctx.node || ctx.milestone) + ':' + r.digest.slice(0, 12),
    })), { keys: wave.map((r) => r.digest.slice(0, 12)), label: 'explore' })
    wave.forEach((r, i) => {
      const call = calls[i]
      if (!call || call.status !== 'completed') {
        if (!failed) failed = call ? { callId: call.callId, status: call.status, reason: call.reason } : { callId: null, status: 'failed', reason: 'explorer_thrown' }
        return
      }
      found[r.digest] = { answer: call.output.answer, evidence: call.output.evidence, call: call.callId, findings: call.output }
    })
  }
  const answers = plan.routes.map((r) => {
    let answer = r.answer || ''
    let call = ''
    let ev = []
    let findings = null
    if (r.route === 'spawn' && found[r.digest]) {
      answer = found[r.digest].answer
      call = found[r.digest].call
      ev = found[r.digest].evidence
      findings = found[r.digest].findings
    }
    if (r.route === 'reuse' && r.same_as >= 0) {
      const sib = plan.routes[r.same_as]
      if (sib && found[sib.digest]) answer = found[sib.digest].answer
      else if (sib) answer = sib.answer || ''
    }
    return { digest: r.digest, id: r.id, question: r.question, route: r.route, answer, call, evidence: ev, findings }
  })
  await crw.exploreRecord(kind, ctx, answers)
  return {
    kind,
    answers,
    spawned: spawn.length,
    inline: plan.routes.filter((r) => r.route === 'inline'),
    orchestrator: plan.routes.filter((r) => r.route === 'orchestrator'),
    escalate: plan.routes.filter((r) => r.route === 'escalate'),
    failed,
  }
}

// exploreEvidence is SP-EXPL-3: a verdict with a defect of kind evidence_missing sends its evidence_question to an explorer
// before the node's next round; its FINDINGS go to the next fix packet and the next reviewer packet. A verdict with no such defect
// records the skip. The finding is never interpreted again by a model. ctx.head is the committed head the reviewer judged: the
// explorer's read-only worktree is made at it, so the evidence is about the revision the question was asked of.
export async function exploreEvidence(node, verdict, ctx) {
  const questions = verdict.defects
    .filter((d) => d.kind === 'evidence_missing' && d.evidence_question)
    .map((d) => ({ id: d.ref, question: d.evidence_question.question, scope: d.file, files_needed: d.evidence_question.files_needed }))
  return explore('evidence', questions, Object.assign({ node }, ctx || {}))
}

// review is the reviewer call of a round and its record: a fresh reviewer thread reads the committed head (SP-REV-1) and answers
// VERDICT, and crw.review's record decides the state of the answer. A reviewer whose answer is null_model is asked once more on a
// fresh thread; the result of that second call is the result. The state is verdict only for a completed, valid VERDICT of a
// reviewer of another model family than the node's authors.
async function review(record, node, plan, head, ci, opts) {
  const round = opts.round || 1
  const packet = reviewPacket(node, plan, head, ci, opts)
  await crw.spawnDecision('REV_1', 'spawn', { node, role: 'reviewer', inputs: { round, head: head.commit } })
  let call = await agent.call(packet, { agentType: 'reviewer', node, head: head.commit, schema: VERDICT, label: 'review:' + node + ':r' + round })
  let rv = await record(node, head, plan.criteria_digest, call, {})
  if (rv.state === 'null_model') {
    call = await agent.call(packet, { agentType: 'reviewer', node, head: head.commit, schema: VERDICT, label: 'review:' + node + ':r' + round + ':retry' })
    rv = await record(node, head, plan.criteria_digest, call, { retried: true })
  }
  return rv
}

// reviewHalt takes the review results that are not a verdict, which are never an executor fix and never tier evidence: held and
// unknown hold the node, refused (independence_refused included) escalates as a configuration fault, null_model - asked twice
// already - escalates, superseded stops.
async function reviewHalt(node, rv) {
  const call = rv.call.callId
  switch (rv.state) {
    case 'held':
      await crw.hold(node, { reason: rv.reason || 'host_infra_unknown', call, source: 'review' })
      return { node, state: 'held', reason: rv.reason, call }
    case 'unknown':
      await crw.hold(node, { reason: 'dispatch_unknown', call, source: 'review' })
      return { node, state: 'held', reason: 'dispatch_unknown', call }
    case 'refused': {
      const esc = await crw.escalate(node, { reason: rv.reason || 'reviewer_refused', call, ask: 'configuration' })
      return { node, state: 'escalated', reason: rv.reason || 'reviewer_refused', call, escalation: esc.id }
    }
    case 'null_model': {
      const esc = await crw.escalate(node, { reason: 'reviewer_null_model', call, detail: rv.reason || '' })
      return { node, state: 'escalated', reason: 'reviewer_null_model', call, escalation: esc.id }
    }
    default:
      return { node, state: 'superseded', call }
  }
}

function refusal(e) {
  const m = /refused:([a-z_0-9]+):/.exec((e && e.message) || '')
  return m ? m[1] : null
}

// land is the land of a verified head: crw.accept assembles the premerge record from the verdict and asks the relay's agent
// acceptance (dag-accept), then crw.integrate lands the accepted head (dag-integrate). A refusal of either - a finding that needs a
// disposition and is blocking by default, a reviewer the relay does not accept - is the result `land_refused` with its closed
// reason, not an exception; any other failure is thrown.
async function land(node, o) {
  let acc
  try {
    acc = await crw.accept(node, o.head)
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
async function tierStep(mint, node, ev) {
  const step = await mint(node, ev)
  const recorded = await crw.stepRecord(node, step, { from: ev.tier, to: 'executor-t1', letter: ev.letter, evidence: ev })
  // the id of the stored decision: the engine's id, or the one the record gave it when that id stood on another checkpoint
  return { id: recorded.id, checkpoint: recorded.checkpoint }
}

// install makes the composites members of crw. The operation the host registered as crw.review is the record of a review, and the
// engine's crw.tierStep mints the id of a step; the composites replace them, calling them for the part they own.
export function install(host) {
  const record = host.review
  const mint = host.tierStep
  host.tierStep = (node, ev) => tierStep(mint, node, ev)
  host.dag = dag
  host.admissionRegions = admissionRegions
  host.explore = explore
  host.exploreEvidence = exploreEvidence
  host.review = (node, plan, head, ci, opts) => review(record, node, plan, head, ci, opts || {})
  host.reviewHalt = reviewHalt
  host.land = land
  host.evidence = evidence
  host.skip = (rule, info) => host.spawnDecision(rule, 'skip', info || {})
  return host
}
