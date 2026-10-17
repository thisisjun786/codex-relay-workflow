import { FINDINGS, FINDINGS_END } from 'crw:lib/schemas'
import { explorerPacket } from 'crw:lib/packets'

// The explorers an SP-EXPL-1 fan-out runs at once.
export const EXPLORER_CONCURRENCY = 3

function chunks(list, n) {
  const out = []
  for (let i = 0; i < list.length; i += n) out.push(list.slice(i, i + n))
  return out
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
  // Keep the inclusive total for existing callers and the input/cache/output split for the design cap.
  const tokenUsage = { input: 0, cached_input: 0, output: 0 }
  const addTokens = (call) => {
    const usage = (call && call.meta && call.meta.tokens) || {}
    tokens += usage.total || 0
    tokenUsage.input += usage.input || 0
    tokenUsage.cached_input += usage.cached_input || 0
    tokenUsage.output += usage.output || 0
  }
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
    addTokens(call)
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
        addTokens(call)
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
        addTokens(call)
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
    token_usage: tokenUsage,
  }
}
