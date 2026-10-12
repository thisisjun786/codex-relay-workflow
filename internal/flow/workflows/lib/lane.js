// crw:lib/lane: the composites of the project lane. Each is a sequence of agent calls and crw operations that the script would
// otherwise repeat at every site, written once so that the sequence is the same everywhere: crw.dag (the lane scheduler),
// crw.review (the reviewer call and its record), crw.reviewHalt, crw.explore and crw.exploreEvidence (the explorer fan-out and its
// record), crw.land (accept, then integrate), crw.evidence (the tier evidence of a node) and crw.milestoneHalt (a milestone that
// cannot go on).
//
// crw.dag also runs the signal path of a milestone (CRW-1202) when it is given an onSignal function: every RECEIPT of a lane is a
// trigger, the triggers are taken in the journal's order into one signal at a time, and onSignal decides what the signal comes to
// (continue, refresh or halt). The signal path is described above dag.
//
// install(crw) makes them members of the crw object, next to the operations the host registers (internal/flow/ops): the script
// then reads as the design writes it, `crw.review(...)`, `crw.land(...)`. A composite is not an operation of its own: it takes no
// position of its path, and every position it uses is that of a call or of an operation it makes, so a resume replays it through
// the journal like any other code of the script. It reads no variable outside its arguments.

import { FINDINGS, VERDICT } from 'crw:lib/schemas'
import { evidence } from 'crw:lib/evidence'
import { explorerPacket, reviewPacket } from 'crw:lib/packets'
import { closeMilestone, settleReviews } from 'crw:lib/close'

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

// The signal path of crw.dag (engine.resume.signals). A lane reports the RECEIPT of each of its executor calls (sig.receipt): that is
// a trigger, with the digest of the call's result and its place in the journal (crw.callFacts). The plan revision a lane reads before its
// release (crw.planRead, sig.plan) is a trigger too when it is another than the plan the audited DESIGN stands on (the plan the current
// revision was made for, or the one a later signal judged and kept it for). One signal path runs at a time, on
// the path `signals:<milestone>` of the join of the dag; the triggers that arrive while it runs are taken together by the next one.
// A signal is a position sequence of its own - crw.signalBind (its input: the triggers, in the order of the journal, and the result
// digest of the signal before it), a join with one item, the child path `signal:<milestone>:<k>`, which runs onSignal, and
// crw.signalDone (its result: continue, refresh or halt, and the revision it made) - so a resume replays it like any other
// position: when every trigger position replayed with the result digest it answered then, the signal before it replayed, and the
// effects hold. Its membership is what the row recorded (crw.recordedSignal), not the order this process saw the calls finish in. The
// first signal that does not replay, and every signal after it, are the suffix: the revisions they made are superseded, and the
// triggers that did replay re-enter the first live signal, in the journal's order.
//
// While a signal is being decided no lane is released (a lane that has not released waits; a lane that has goes on) and no node is
// admitted, because a re-plan that is due changes the packet and the admission regions of the lanes that have not started. A lane that a
// resume finds released under a revision a signal made waits until that signal is decided, so the release finds the revision it stands on
// (crw.release). Until the signals the earlier process recorded are decided, only the nodes they concern are admitted: any other node is
// released after them, under the revision that stands then. A recorded trigger whose lane ran a call live does not come any more. A lane
// whose plan read was a trigger (its own, or the same revision another lane brought first) waits until the signal that took that trigger
// is decided, whether that signal runs live or is replayed: while a recorded signal waits for its triggers no lane is held otherwise.
function signalState(planId, opts, nodes) {
  const S = { queue: [], arrived: {}, taken: {}, takenAt: {}, planWait: {}, planSignal: {}, finished: {}, live: {}, remaining: nodes.length, lanesDone: nodes.length === 0, halted: false, active: false, replaying: false,
    processed: 0, liveFrom: 0, over: false, waiters: [], recordedPending: !!crw.recordedSignal(opts.milestone, 1), allowed: {}, repump() {}, applyRegions() {} }
  S.notify = () => {
    const ws = S.waiters
    S.waiters = []
    for (const w of ws) w()
  }
  S.wait = () => new Promise((resolve) => { S.waiters.push(resolve) })
  S.holds = () => !S.over && (S.active || (S.queue.length > 0 && !S.replaying))
  // the signal j of a revision is decided when it was replayed, when the first signal that did not replay has ended (a revision
  // it supersedes is then known to be, and the revision that stands can be compared with), or when the signal path is over
  S.decided = (j) => j <= 0 || S.processed >= j || (S.liveFrom > 0 && S.processed >= S.liveFrom) || S.over
  S.laneFinished = (node) => {
    S.finished[node] = true
    if (--S.remaining <= 0) S.lanesDone = true
    S.notify()
  }
  // the earlier process's signals are all decided (replayed, or the first that did not replay has begun): the nodes that no signal
  // of it concerned may be released
  S.endRecorded = () => {
    if (!S.recordedPending) return
    S.recordedPending = false
    S.repump()
  }
  // a trigger reached the signal path; a lane that ran a call live is live from there on, and a recorded trigger of it that has not
  // come cannot come any more
  S.arrive = (t) => {
    const seen = S.arrived[t.call]
    if (seen) {
      if (t.seq < seen.seq) seen.seq = t.seq
      return
    }
    S.arrived[t.call] = t
    if (t.live) S.live[t.node] = true
    S.queue.push(t)
    S.notify()
  }
  S.forLane = (node) => ({
    receipt(res) {
      if (!res || res.status !== 'completed') return
      const f = crw.callFacts(res)
      S.arrive({ kind: 'receipt', call: f.callId, node, digest: f.digest, live: f.live, seq: f.seq })
    },
    // a plan revision the lane read (crw.planRead) that is not the one the audited DESIGN stands on is a trigger, once
    plan(read) {
      // a plan a signal in flight already read is no trigger of its own: the release waits until that signal is decided
      S.planSignal[node] = read && !read.changed && read.judging > 0 ? read.judging : 0
      if (!read || !read.changed) return
      const f = crw.callFacts(read)
      const call = read.occurrence ? 'plan:' + read.occurrence + ':' + read.digest : 'plan:' + read.digest
      S.planWait[node] = call
      S.arrive({ kind: 'revision', call, node, digest: read.digest, live: f.live, seq: f.seq })
    },
  })
  return S
}

const noSignals = { receipt() {}, plan() {} }

// the options of every release of a lane: crw.release compares the live plan with the plan the audited DESIGN stands on, in the flow,
// before Relay.Release (the relay's release commit does not compare the plan slice; CRW-1288)
const PINNED = { pinned: true }

// admit holds a lane back from its release while a signal is being decided, for a lane that an earlier process released under a
// revision a signal made until that signal is decided, and for a lane whose plan read was a trigger until the signal that took the
// trigger is decided, or until the signal in flight that read the very plan it read is decided. ask counts the lane's asks for its
// release: the recorded release is the answer of the ask after its plan_changed refusals, and only that ask waits for the signal of its
// revision. It answers false when the milestone was halted.
async function admit(S, node, ask) {
  // the recorded release is the answer of the ask after the plan_changed refusals before it; an earlier ask replays its refusal
  const recorded = crw.priorRelease(node)
  const prior = recorded && (ask || 0) >= (recorded.refused || 0) ? recorded : null
  const planDecided = () => {
    const call = S.planWait[node]
    return (!call || S.over || (S.takenAt[call] > 0 && S.processed >= S.takenAt[call])) && S.decided(S.planSignal[node] || 0)
  }
  while (true) {
    if (S.halted) return false
    if (!S.holds() && (!prior || S.decided(prior.signal)) && planDecided()) return true
    await S.wait()
  }
}

// nextAttempt is the attempt of the live signal that takes over a recorded row bound with attempt a: '' (the first process) and 'live'
// (a row bound before attempts were numbered) count as 0 and 1.
function nextAttempt(a) {
  const m = /^live(?::(\d+))?$/.exec(a || '')
  const n = !a || !m ? 0 : m[1] ? Number(m[1]) : 1
  return 'live:' + (n + 1)
}

function plainTrigger(t) {
  return { kind: t.kind, call: t.call, node: t.node, digest: t.digest }
}

// The result of onSignal: a string, or {result, revision, reasons, regions}; regions maps a node to the admission regions it has under
// the revision the signal made.
function signalOutcome(v) {
  if (typeof v === 'string') return { result: v, revision: '', reasons: [] }
  if (v && typeof v.result === 'string') return { result: v.result, revision: v.revision || '', reasons: v.reasons || [], regions: v.regions || null }
  return { result: 'halt', revision: '', reasons: ['onSignal_returned_nothing'] }
}

async function signalPath(S, ms, onSignal) {
  let prev = ''
  let k = 1
  try {
    for (; ; k++) {
      // the first signal that did not replay, and every signal after it, are live: none of them is looked up in the earlier process's
      // record
      const rec = S.liveFrom > 0 ? null : crw.recordedSignal(ms, k)
      let members = null
      let replay = false
      if (!rec) S.endRecorded()
      if (rec) {
        // the signal the earlier process made: it waits for the triggers it was made of, and is replayed when each of them was
        // replayed and answered the digest it answered then. A trigger is settled when it came, when its lane ran a call live (what
        // it does after that is live, so a recorded trigger of it that has not come does not come) or when its lane has ended.
        S.replaying = true
        while (!rec.members.every((m) => S.arrived[m.call] || S.live[m.node] || S.finished[m.node]) && !S.halted) await S.wait()
        replay = rec.members.every((m) => {
          const a = S.arrived[m.call]
          return a && a.live === false && a.digest === m.digest && !S.taken[m.call]
        })
        if (replay) members = rec.members.map(plainTrigger)
        S.replaying = false
      } else {
        while (S.queue.length === 0 && !S.lanesDone && !S.halted) await S.wait()
      }
      if (!members) members = S.queue.filter((t) => !S.taken[t.call]).sort((a, b) => a.seq - b.seq).map(plainTrigger)
      if (members.length === 0 || S.halted) break
      S.active = true
      if (!replay) S.endRecorded()
      for (const m of members) {
        S.taken[m.call] = true
        S.takenAt[m.call] = k
      }
      S.queue = S.queue.filter((t) => !S.taken[t.call])
      // a recorded signal that does not replay (one of its triggers ran again) is bound as a live attempt: its triggers can answer the very
      // digests they answered before, and the row of the earlier process must still not replay. The attempt is one more than the attempt
      // of the row it does not replay (live:<n>), so a resume after a resume that ran a trigger again does not hand the row that resume
      // bound live its own arguments. A row an earlier resume bound live replays with the attempt it was bound with: its triggers
      // answer the digests they answered.
      const attempt = rec && !replay ? nextAttempt(rec.attempt) : rec && rec.attempt ? rec.attempt : ''
      const bound = attempt ? await crw.signalBind(ms, k, prev, members, attempt) : await crw.signalBind(ms, k, prev, members)
      const ran = await parallel([() => onSignal(bound)], { keys: ['signal:' + ms + ':' + k], label: 'signal' })
      const out = signalOutcome(ran[0])
      const done = await crw.signalDone(ms, k, { result: out.result, revision: out.revision, reasons: out.reasons })
      if (out.regions) S.applyRegions(out.regions)
      prev = done.digest
      S.processed = k
      if (!replay && S.liveFrom === 0) S.liveFrom = k
      if (out.result === 'halt') S.halted = true
      S.active = false
      S.notify()
      S.repump()
    }
    const closing = await crw.signalClose(ms, { signals: k - 1, digest: prev })
    return { signals: k - 1, digest: prev, superseded: closing.superseded }
  } finally {
    S.active = false
    S.over = true
    S.recordedPending = false
    S.notify()
  }
}

// dag runs the lane function of every node of a milestone: in the order the scheduler offers the nodes (the order of
// opts.nodes), a node starting as soon as the nodes it waits for have landed, no more than opts.cap nodes at once, and never two
// nodes whose edit regions overlap. There is no barrier: a node starts when it can, not when a wave ends. Each node runs on a
// path of its own (its id), starts with crw.release - the scheduler's admission and the slot - and ends with the lane's result.
// A node whose predecessor did not land is not started: its result is blocked. The result is the list of the lanes' results in
// the order of opts.nodes. The lane function is called with the node, its release and an object whose receipt(result) reports the
// RECEIPT of an executor call to the signal path (opts.onSignal; a dag without one has none). A node reads the plan (crw.planRead) before
// its release when there is a signal path, and every release compares the plan with the one the audited DESIGN stands on.
export async function dag(planId, lane, opts) {
  const nodes = opts.nodes
  const cap = opts.cap > 0 ? opts.cap : DEFAULT_CAP
  const onSignal = typeof opts.onSignal === 'function' ? opts.onSignal : null
  const S = onSignal ? signalState(planId, opts, nodes) : null
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
  if (S && S.recordedPending) {
    // until the signals the earlier process recorded are decided, only the nodes they concern start: the lanes that were released
    // (and the lanes they wait for) and the lanes whose triggers the signals are made of. The others are released after, under the
    // revision that stands then.
    const allow = (id) => {
      if (S.allowed[id] || !byId[id]) return
      S.allowed[id] = true
      for (const d of byId[id].after || []) allow(d)
    }
    for (const n of nodes) if (crw.priorRelease(n.id)) allow(n.id)
    for (let k = 1; ; k++) {
      const rec = crw.recordedSignal(opts.milestone, k)
      if (!rec) break
      for (const m of rec.members) allow(m.node)
    }
  }
  const active = () => nodes.filter((n) => state[n.id] === 'running')
  // A node holds its regions from the moment it is past the signal gate and about to be released. The regions of a node that has not
  // held them yet are the ones of the revision that stands when it does: a signal that made another revision may have changed them
  // after the scheduler admitted the node, so the node is checked against the nodes that hold theirs once more.
  const claimed = {}
  const clashes = (n) => nodes.some((o) => o !== n && claimed[o.id] && state[o.id] === 'running' && regionsOverlap(o.regions, n.regions))
  async function claim(n, ask) {
    while (true) {
      if (!(await admit(S, n.id, ask)) || S.halted) return false
      if (S.holds()) continue
      if (!clashes(n)) {
        claimed[n.id] = true
        return true
      }
      await S.wait()
    }
  }
  function pump() {
    let again = true
    while (again) {
      again = false
      for (const n of nodes) {
        if (state[n.id] !== 'pending') continue
        if (S && S.halted) {
          // a milestone that was halted starts no more nodes
          state[n.id] = 'ended'
          blockedBy[n.id] = ['signal_halt']
          gates[n.id].open(false)
          continue
        }
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
        if (S && S.recordedPending && !S.allowed[n.id]) continue
        // no node is admitted while a signal is decided: what it makes (a revision, the regions of its nodes) is what the admission reads
        if (S && S.holds()) continue
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
    if (!go) {
      if (S) S.laneFinished(n.id)
      return { node: n.id, state: 'blocked', blocked_by: blockedBy[n.id] }
    }
    let out
    try {
      // Every release compares the plan with the plan the audited DESIGN stands on (crw.release, pinned) before the relay is asked: a
      // plan that is another at that comparison releases nothing; a plan that changes after it, before the relay's release commit, is
      // not caught there (CRW-1288). With a signal path the lane then reads the plan again - another plan is a trigger,
      // and the release waits for the signal that comes of it, which re-plans and audits a delta for that plan or keeps the revision -
      // and asks again; without one the node is not released.
      let rel = null
      for (let ask = 0; ; ask++) {
        if (S) {
          S.forLane(n.id).plan(await crw.planRead(opts.milestone, n.id))
          if (!(await claim(n, ask))) break
        }
        rel = await crw.release(planId, n.id, opts.milestone, PINNED)
        if (!S || rel.released || rel.reason !== 'plan_changed') break
        // the node holds no regions while it waits: the revision a re-plan makes may give it others
        claimed[n.id] = false
        rel = null
      }
      if (!rel) {
        out = { node: n.id, state: 'halted', reason: 'signal_halt' }
      } else {
        if (!rel.released && rel.kind === 'executor') out = { node: n.id, state: 'unreleased', reason: rel.reason }
        else out = await lane(n, rel, S ? S.forLane(n.id) : noSignals)
      }
    } catch (e) {
      state[n.id] = 'ended'
      if (S) S.laneFinished(n.id)
      pump()
      throw e
    }
    state[n.id] = out && out.state === 'landed' ? 'landed' : 'ended'
    if (S) S.laneFinished(n.id)
    pump()
    return out
  })
  if (S) {
    S.repump = pump
    S.applyRegions = (regions) => {
      for (const n of nodes) if ((state[n.id] === 'pending' || (state[n.id] === 'running' && !claimed[n.id])) && regions[n.id]) n.regions = regions[n.id]
      pump()
    }
    thunks.push(async () => {
      try {
        return await signalPath(S, opts.milestone, onSignal)
      } finally {
        pump()
      }
    })
  }
  const keys = nodes.map((n) => n.id)
  if (S) keys.push('signals:' + opts.milestone)
  pump()
  const results = await parallel(thunks, { keys, label: 'dag' })
  return S ? results.slice(0, nodes.length) : results
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

// milestoneHalt takes a call of the design phase, of the close or of a re-plan that did not complete, or a gate that failed. held and
// unknown hold the milestone (a resume goes on when the cause is gone, and runs the same call again as a new turn), superseded
// stops, and everything else - refused, null_model, a gate that failed or cannot be settled, a QA that is INCONCLUSIVE - escalates.
// A call that did not complete is never read as an approval, a design, a gate, a review or an alignment.
async function milestoneHalt(ms, source, status, reason, callId) {
  const node = 'milestone:' + ms.id
  if (status === 'held' || status === 'unknown') {
    await crw.hold(node, { reason: status === 'unknown' ? 'dispatch_unknown' : (reason || 'host_infra_unknown'), call: callId, source })
    return { state: 'held', source, reason, call: callId }
  }
  if (status === 'superseded') return { state: 'superseded', source, call: callId }
  const esc = await crw.escalate(null, { reason: source + '_' + status, call: callId, detail: reason || '' })
  return { state: 'escalated', source, reason: reason || status, call: callId, escalation: esc.id }
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
  host.milestoneHalt = milestoneHalt
  host.land = land
  host.closeMilestone = closeMilestone
  host.settleReviews = settleReviews
  host.evidence = evidence
  host.skip = (rule, info) => host.spawnDecision(rule, 'skip', info || {})
  return host
}
