// crw:lib/lane: the composites of the project lane. Each is a sequence of agent calls and crw operations that the script would
// otherwise repeat at every site, written once so that the sequence is the same everywhere: crw.dag (the lane scheduler), crw.explore
// (the explorer fan-out and its record), crw.land (accept, then integrate), crw.evidence (the tier evidence of a node) and
// crw.milestoneHalt (a milestone that cannot go on). A node is judged by its unit check (crw.unitCheck, an operation of the host), and
// no composite calls a reviewer agent (CRW-1447).
//
// crw.dag also runs the signal path of a milestone (CRW-1202) when it is given an onSignal function: every RECEIPT of a lane is a
// trigger, the triggers are taken in the journal's order into one signal at a time, and onSignal decides what the signal comes to
// (continue, refresh or halt). The signal path is described above dag.
//
// install(crw) makes them members of the crw object, next to the operations the host registers (internal/flow/ops): the script
// then reads as the design writes it, `crw.dag(...)`, `crw.land(...)`. A composite is not an operation of its own: it takes no
// position of its path, and every position it uses is that of a call or of an operation it makes, so a resume replays it through
// the journal like any other code of the script. It reads no variable outside its arguments.

import { FINDINGS, FINDINGS_END } from 'crw:lib/schemas'
import { evidence } from 'crw:lib/evidence'
import { explorerPacket } from 'crw:lib/packets'
import { closeMilestone } from 'crw:lib/close'

// The executors a project runs at once (docs: at most three, and never more than the region-disjoint ready nodes).
export const DEFAULT_CAP = 3
// The explorers an SP-EXPL-1 fan-out runs at once.
export const EXPLORER_CONCURRENCY = 3

function chunks(list, n) {
  const out = []
  for (let i = 0; i < list.length; i += n) out.push(list.slice(i, i + n))
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
    processed: 0, liveFrom: 0, over: false, waiters: [], recordedPending: !!crw.recordedSignal(opts.milestone, 1), allowed: {}, repump() {}, async applyRegions() {} }
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
// before Relay.Release, whose release commit compares the node's slice of that plan again (CRW-1288)
const PINNED = { pinned: true }

// the reason of a release the relay held back because its edit regions overlap regions a node it holds released has (dagsched)
const DEFER_EDIT_OVERLAP = 'defer:edit_overlap'

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
      // the regions of the revision are judged against the others (crw.regionsOverlap) while the signal still holds every admission
      if (out.regions) await S.applyRegions(out.regions)
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
// Two nodes overlap as crw.regionsOverlap judges their admission regions (the relay's own rule, with the plan's change and hold, and
// Scope's reading of a region; CRW-1461): before the first node is admitted, every pair of nodes neither of which waits for the other
// is judged once, in the order of opts.nodes, and a signal whose revision gives a node other regions judges the new list against the
// lists every other node has had before it admits anything. The answers are kept by the content of the two lists, so the scheduler
// reads them without an operation of its own, and a pair it has no answer for overlaps. Every judgment is an operation on the path of
// dag's caller or of the signal path, made in an order and with arguments that only the plan, the DESIGN and the signals' results
// decide, never the order the lanes ran in.
// A node the relay held back for an edit overlap that a node of the dag may hold is pending again until another node has ended
// (CRW-1467, in the lane below). A node whose predecessor did not land is not started: its result is blocked. The result is the list of the lanes' results in
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
  const newGate = () => {
    const gate = {}
    gate.promise = new Promise((resolve) => { gate.open = resolve })
    return gate
  }
  for (const n of nodes) {
    byId[n.id] = n
    state[n.id] = 'pending'
    gates[n.id] = newGate()
  }
  // ended counts the lanes that ended after they ran; deferredAt is, for a node the relay held back for an edit overlap that another
  // node of the dag may hold (it is pending again), the count of the node's ask: it is admitted again once a node ended after that ask
  // (-1 for an answer the record asked again after: it waits for no end)
  let ended = 0
  const deferredAt = {}
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
  // the nodes a node waits for, through any chain: two nodes of which one waits for the other never run at once and are not judged
  const before = {}
  const waitsFor = (id) => {
    if (before[id]) return before[id]
    const out = {}
    for (const d of byId[id].after || []) {
      if (!byId[d]) continue
      out[d] = true
      Object.assign(out, waitsFor(d))
    }
    before[id] = out
    return out
  }
  const related = (a, b) => !!(waitsFor(a)[b] || waitsFor(b)[a])
  // the overlap of two region lists, by their content (crw.regionsOverlap); versions are the lists each node has had, in order
  const table = {}
  const versions = {}
  const rk = (list) => JSON.stringify(list || [])
  const pairKey = (a, b) => {
    const x = rk(a)
    const y = rk(b)
    return x < y ? x + '\u0000' + y : y + '\u0000' + x
  }
  const overlaps = (a, b) => {
    const v = table[pairKey(a, b)]
    return v === undefined ? true : v === true
  }
  const judge = async (a, b) => {
    const key = pairKey(a, b)
    if (!(key in table)) table[key] = await crw.regionsOverlap(a || [], b || [])
  }
  for (const n of nodes) versions[n.id] = [n.regions]
  for (let i = 0; i < nodes.length; i++) {
    for (let j = i + 1; j < nodes.length; j++) {
      if (!related(nodes[i].id, nodes[j].id)) await judge(nodes[i].regions, nodes[j].regions)
    }
  }
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
  const clashes = (n) => nodes.some((o) => o !== n && claimed[o.id] && state[o.id] === 'running' && overlaps(o.regions, n.regions))
  // seen is the node's held-back answers (defer:edit_overlap) so far: the recorded release is the answer of the ask after the recorded ones
  // too (crw.deferredAsks), so an ask before them, which replays one of them, does not wait for the signal of the recorded release's
  // revision (that signal may wait for the plan read after a held-back answer)
  async function claim(n, ask, seen) {
    const recordedAsk = seen >= crw.deferredAsks(n.id) ? ask : -1
    while (true) {
      if (!(await admit(S, n.id, recordedAsk)) || S.halted) return false
      if (S.holds()) continue
      if (!clashes(n)) {
        claimed[n.id] = true
        return true
      }
      await S.wait()
    }
  }
  // the order the pump offers the nodes in: the scheduler's, a node that is pending again after a held-back release last
  const offered = () => nodes.filter((n) => deferredAt[n.id] === undefined).concat(nodes.filter((n) => deferredAt[n.id] !== undefined))
  function pump() {
    let again = true
    while (again) {
      again = false
      for (const n of offered()) {
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
        // a node held back for an edit overlap waits until another node has ended since it asked
        if (deferredAt[n.id] !== undefined && ended <= deferredAt[n.id]) continue
        // a replayed re-admission (-1) comes after every pending node that overlaps it and does not wait for it: that node may be the one
        // that made the signal the re-admitted node's claim waits for, and it cannot enter beside the node (CRW-1467)
        if (deferredAt[n.id] === -1 && nodes.some((o) => o !== n && state[o.id] === 'pending' && deferredAt[o.id] === undefined && !waitsFor(o.id)[n.id] && overlaps(o.regions, n.regions))) continue
        // no node is admitted while a signal is decided: what it makes (a revision, the regions of its nodes) is what the admission reads
        if (S && S.holds()) continue
        const running = active()
        if (running.length >= cap) continue
        if (running.some((r) => overlaps(r.regions, n.regions))) continue
        state[n.id] = 'running'
        delete deferredAt[n.id]
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
      // Every release compares the plan with the plan the audited DESIGN stands on (crw.release, pinned) before the relay is asked, and
      // the relay's release commit compares the node's slice of that plan again (CRW-1288): a plan that is another at either releases
      // nothing (plan_changed). With a signal path the lane then reads the plan again - another plan is a trigger,
      // and the release waits for the signal that comes of it, which re-plans and audits a delta for that plan or keeps the revision -
      // and asks again; without one the node is not released. A relay refusal while the plan the flow reads again is still the pinned
      // one (relay_plan_changed) raises the run-level hold plan_superseded (CRW-1291): the run does not follow the relay's new revision.
      //
      // A release the relay held back for an edit overlap (defer:edit_overlap) while another node of this dag held the regions is the
      // dag's own doing (CRW-1467): the relay judges by the same rule as the dag, but it sees the nodes it holds released, which the dag
      // admitted beside this one. The node gives up its claim and asks again: at once when another node ended since it asked (that node may
      // be the one that held it back, whether it ended before or after the answer came and before the lane read it), otherwise pending
      // again - it holds no slot of the cap and no regions - until another node has ended. A resume reads, from the record, the held-back
      // answers the lane asked again after (crw.deferredAsks) and asks again after each of them as the record did, whatever the order the
      // other lanes replay in: the node is pending again, and the pump takes it after the other pending nodes as soon as the cap and the
      // regions let it, waiting for no end. A node held back while no other node runs and none ended since it asked is held back by something outside
      // the dag: it is not released (unreleased), as before. Each re-ask after the record's needs another node to have ended, so a node
      // asks again at most once for each other node.
      let rel = null
      let seen = 0
      for (let ask = 0; ; ) {
        if (S) {
          S.forLane(n.id).plan(await crw.planRead(opts.milestone, n.id))
          if (!(await claim(n, ask, seen))) break
        }
        const askedAt = ended
        rel = await crw.release(planId, n.id, opts.milestone, PINNED)
        if (!rel.released && rel.reason === DEFER_EDIT_OVERLAP && !rel.hold && rel.kind === 'executor') {
          const recorded = seen < crw.deferredAsks(n.id)
          const others = nodes.some((o) => o !== n && state[o.id] === 'running')
          if (!recorded && !others && ended === askedAt) break
          seen++
          claimed[n.id] = false
          rel = null
          if (S) S.notify()
          if (recorded || ended === askedAt) {
            // pending again: the node holds no slot of the cap and no regions, and the pump takes it after the other pending nodes - a
            // replayed answer as soon as the cap and the regions let it (a node that ran in its place, and the signal it made, replay
            // first), a live one once another node has ended since its ask
            deferredAt[n.id] = recorded ? -1 : askedAt
            gates[n.id] = newGate()
            state[n.id] = 'pending'
            pump()
            if (!(await gates[n.id].promise)) break
          }
          continue
        }
        if (!S || rel.released || rel.reason !== 'plan_changed') break
        // the node holds no regions while it waits: the revision a re-plan makes may give it others
        claimed[n.id] = false
        rel = null
        ask++
      }
      if (!rel) {
        out = { node: n.id, state: 'halted', reason: 'signal_halt' }
      } else if (!rel.released && rel.hold) {
        out = await superseded(n.id, rel)
      } else {
        // a release the relay refused for a lost epoch carries the relay's detail, which the run's stop names (CRW-1382)
        if (!rel.released && rel.kind === 'executor') out = rel.detail ? { node: n.id, state: 'unreleased', reason: rel.reason, detail: rel.detail } : { node: n.id, state: 'unreleased', reason: rel.reason }
        else out = await lane(n, rel, S ? S.forLane(n.id) : noSignals)
      }
    } catch (e) {
      state[n.id] = 'ended'
      ended++
      if (S) S.laneFinished(n.id)
      pump()
      throw e
    }
    state[n.id] = out && out.state === 'landed' ? 'landed' : 'ended'
    ended++
    if (S) S.laneFinished(n.id)
    pump()
    return out
  })
  if (S) {
    S.repump = pump
    // a revision's regions: every list a node has not had yet is judged, in the order of the nodes, against each list every other node
    // it may run beside has had, whatever the node's state (so the judgments are what the signal's result decides); then the nodes that
    // hold no regions yet take theirs
    S.applyRegions = async (regions) => {
      const fresh = []
      for (const n of nodes) {
        const list = regions[n.id]
        if (!list || versions[n.id].some((v) => rk(v) === rk(list))) continue
        versions[n.id].push(list)
        fresh.push({ n, list })
      }
      for (const f of fresh) {
        for (const o of nodes) {
          if (o === f.n || related(f.n.id, o.id)) continue
          for (const v of versions[o.id]) await judge(f.list, v)
        }
      }
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

// superseded is a node under the run-level hold plan_superseded (CRW-1291): the relay's head moved past the plan revision the run pinned,
// and the run does not follow it. Nothing restarts on its own: the node whose release raised the hold waits for the parent's decision on
// it, which closes the node by escalation; every other node stays unreleased while the hold stands. Ending or cancelling the run and
// starting a new one on the new revision is the other way out.
async function superseded(node, rel) {
  if (rel.escalation) {
    const d = await crw.decide(node, { id: rel.escalation })
    if (d.applied) {
      const esc = await crw.escalate(node, { reason: 'plan_superseded', detail: 'closed by the decision on the hold ' + rel.escalation })
      return { node, state: 'escalated', reason: 'plan_superseded', escalation: esc.id }
    }
  }
  return { node, state: 'held', reason: 'plan_superseded', hold: rel.hold }
}

// explore routes the questions of an asker (crw.exploreRoute decides: reuse, inline for a research question, spawn, hand to the
// orchestrator - an open question of the DESIGN beyond the explorer cap or with an out-of-authority scope notation -, skip),
// runs the explorers it spawned at most EXPLORER_CONCURRENCY at a time, and records the answers (crw.exploreRecord), so a
// question of the same digest asked again in the run reuses the answer and names the route it came from (origin). The result lists an answer for each question, the
// questions that went to the orchestrator thread or that exhausted the asker's research, the call that failed when an
// explorer did not complete, and the tokens the explorers it spawned report (meta.tokens: a design loop counts them, CRW-1327).
//
// An explorer whose answer is null_model is asked once more on a fresh thread (CRW-1571): the same packet, schema, node and head,
// labelled with :retry, in a join of its own after its wave (explore-retry), so a fan-out in which no explorer fails makes the calls it
// made before. A question whose explorer is null_model twice is missing: its answer stays empty with no call, as before, and the
// answer crw.explore returns carries missing ({calls, status, reason}), which crw.exploreRecord is not given (its arguments are the
// ones before CRW-1571). The fan-out has failed for it only when every explorer it spawned is missing (failed: the last retry); any
// other status that is not completed (held, unknown, superseded, refused, a thrown call) is not asked again and is the failure, as
// before.
//
// opts.endConditions are the end condition commands of the milestone's issues ({issue, command}, CRW-1382): the first explorer this
// fan-out spawns is given them to run once, and its answer names them (end_conditions_asked) so crw.exploreRecord can warn of a command
// that failed, ran no test or was not reported. They are not an argument of crw.exploreRoute or of crw.exploreRecord's context, and
// neither is opts.head: the head the explorer calls are placed at when the context names none.
export async function explore(kind, questions, ctx, opts) {
  const plan = await crw.exploreRoute(kind, ctx, questions)
  const spawn = plan.routes.filter((r) => r.route === 'spawn')
  const ends = (opts && opts.endConditions) || []
  const endsFor = spawn.length && ends.length ? spawn[0].digest : null
  const found = {}
  let failed = null
  let tokens = 0
  // a background explorer (CRW-1466) is labelled explore-bg:<milestone>:<digest12>, so a reader of the explore: labels (an open
  // question's, a research or evidence question's) never takes one for its own; every other kind keeps its label
  const prefix = kind === 'background' ? 'explore-bg:' : 'explore:'
  // the head the explorers read: the context's (it is part of the route, and of the reuse key of a re-plan's or an evidence question),
  // else opts.head, a head the calls are placed at that the route does not key on (the first DESIGN's design head)
  const callHead = ctx.head || (opts && opts.head) || undefined
  // the questions whose explorer was null_model twice (CRW-1571), by digest, and the last retry that was
  const missing = {}
  let lastMissing = null
  const label = (r) => prefix + (ctx.node || ctx.milestone) + ':' + r.digest.slice(0, 12)
  const ask = (r, suffix) => r.digest === endsFor
    ? agent.call(explorerPacket(kind, r, ctx, ends), {
      agentType: 'explorer', node: ctx.node || undefined, head: callHead, schema: FINDINGS_END, label: label(r) + suffix,
    })
    : agent.call(explorerPacket(kind, r, ctx), {
      agentType: 'explorer', node: ctx.node || undefined, head: callHead, schema: FINDINGS, label: label(r) + suffix,
    })
  const settle = (r, call) => {
    tokens += (call && call.meta && call.meta.tokens && call.meta.tokens.total) || 0
    if (!call || call.status !== 'completed') {
      if (!failed) failed = call ? { callId: call.callId, status: call.status, reason: call.reason } : { callId: null, status: 'failed', reason: 'explorer_thrown' }
      return
    }
    found[r.digest] = { answer: call.output.answer, evidence: call.output.evidence, call: call.callId, findings: call.output }
  }
  for (const wave of chunks(spawn, EXPLORER_CONCURRENCY)) {
    // the explorer given the end conditions answers FINDINGS_END; every other one is the call it was before CRW-1382 (its packet and
    // its schema), so a run recorded then replays it
    const calls = await parallel(wave.map((r) => () => ask(r, '')), { keys: wave.map((r) => r.digest.slice(0, 12)), label: 'explore' })
    const again = []
    wave.forEach((r, i) => {
      const call = calls[i]
      if (call && call.status === 'null_model') {
        tokens += (call.meta && call.meta.tokens && call.meta.tokens.total) || 0
        again.push({ r, first: call })
        return
      }
      settle(r, call)
    })
    if (!again.length) continue
    const retries = await parallel(again.map((x) => () => ask(x.r, ':retry')), { keys: again.map((x) => x.r.digest.slice(0, 12) + ':retry'), label: 'explore-retry' })
    again.forEach((x, i) => {
      const call = retries[i]
      if (call && call.status === 'null_model') {
        tokens += (call.meta && call.meta.tokens && call.meta.tokens.total) || 0
        missing[x.r.digest] = { calls: [x.first.callId, call.callId], status: call.status, reason: call.reason || '' }
        lastMissing = { callId: call.callId, status: call.status, reason: call.reason }
        return
      }
      settle(x.r, call)
    })
  }
  // every explorer the fan-out spawned is missing: the fan-out failed, as it did before CRW-1571 when one explorer was null_model
  if (!failed && spawn.length && spawn.every((r) => missing[r.digest])) failed = lastMissing
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
    const out = { digest: r.digest, id: r.id, question: r.question, route: r.route, reason: r.reason || '', scope: r.scope || '', authority: r.authority || '', answer, call, evidence: ev, findings }
    // the optional origin only with a value (CRW-1381)
    if (r.origin) out.origin = r.origin
    if (r.route === 'spawn' && r.digest === endsFor) out.end_conditions_asked = ends
    return out
  })
  await crw.exploreRecord(kind, ctx, answers)
  // the answers crw.explore returns: a question whose explorer was null_model twice names it (missing); crw.exploreRecord was given
  // the answers without the member
  const returned = answers.map((a, i) => {
    const r = plan.routes[i]
    const sib = r.route === 'reuse' && r.same_as >= 0 ? plan.routes[r.same_as] : null
    const miss = r.route === 'spawn' ? missing[r.digest] : sib && sib.route === 'spawn' && !found[sib.digest] ? missing[sib.digest] : undefined
    const withMissing = miss ? Object.assign({}, a) : a
    if (miss) withMissing.missing = miss
    return withMissing
  })
  return {
    kind,
    answers: returned,
    spawned: spawn.length,
    inline: plan.routes.filter((r) => r.route === 'inline'),
    orchestrator: plan.routes.filter((r) => r.route === 'orchestrator'),
    escalate: plan.routes.filter((r) => r.route === 'escalate'),
    // the questions the run asked that no answer reached and these questions do not raise again: the next ask carries them (CRW-1381)
    carried: plan.carried || [],
    failed,
    tokens,
  }
}

// milestoneHalt takes a call of the design phase, of the close or of a re-plan that did not complete, or a gate that failed. held and
// unknown hold the milestone (a resume goes on when the cause is gone, and runs the same call again as a new turn), superseded
// stops, and everything else - refused, null_model, a gate that failed or cannot be settled, a QA that is INCONCLUSIVE, a crosscheck
// that did not verify the range - escalates.
// A call that did not complete is never read as an approval, a design, a gate, a review or an alignment. extra is what the stop of
// a close carries ({ask, evidence, fix, detail}; every member optional): the escalation asks extra.ask with extra.evidence, its detail
// adds extra.detail, and the result carries the ask, the fix packet and the crosscheck's V (extra.evidence.crosscheck) as the op
// recorded it, so the run's value hands V on unchanged.
async function milestoneHalt(ms, source, status, reason, callId, extra) {
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
async function land(node, o) {
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
async function tierStep(mint, node, ev) {
  const step = await mint(node, ev)
  const recorded = await crw.stepRecord(node, step, { from: ev.tier, to: 'executor-t1', letter: ev.letter, evidence: ev })
  // the id of the stored decision: the engine's id, or the one the record gave it when that id stood on another checkpoint
  return { id: recorded.id, checkpoint: recorded.checkpoint }
}

// install makes the composites members of crw. The engine's crw.tierStep mints the id of a step; the composite replaces it, calling it
// for the part it owns.
export function install(host) {
  const mint = host.tierStep
  host.tierStep = (node, ev) => tierStep(mint, node, ev)
  host.dag = dag
  host.explore = explore
  host.milestoneHalt = milestoneHalt
  host.land = land
  host.closeMilestone = closeMilestone
  host.evidence = evidence
  host.skip = (rule, info) => host.spawnDecision(rule, 'skip', info || {})
  return host
}
