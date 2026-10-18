import { DESIGN_AUDIT } from 'crw:lib/schemas'
import { answeredQuestions, auditFindings, designAnswersPacket, designAuditPacket, designFixPacket, replanPacket } from 'crw:lib/packets'

// The caps of the design loop (one auditedDesign: the DESIGN it starts from, its audit rounds and the architect's fixes; CRW-1327).
// AUDIT_ROUNDS is the round cap: a DESIGN not approved in that many rounds stops the milestone with the design_audit_rounds
// escalation (CRW-1347). The token cap is DESIGN_TOKEN_CAP tokens, or DESIGN_TOKEN_WARN of the run's token budget when that is lower,
// so a run without a budget has one too: when the calls of the loop (its architect, answers, audit and fix calls and the explorers of its open
// questions) have used it, the loop warns once and tells the orchestrator (design_loop_tokens, no ask) and goes on; the engine itself
// pauses the run at the whole budget. The background explorers that run before the architect are outside the loop and its token cap
// (one explorer for each node bounds them; the engine's budget counts them like every call). Either cap writes a `warn design_loop_cap` log row. The run's budget is budget.run, the one its
// first process started with, which the journal fixes: a resume under another ceiling (budget.total) reaches the cap at the same
// call and raises the escalation with the same input, so the loop replays (CRW-1327).
const AUDIT_ROUNDS = 3
const DESIGN_TOKEN_CAP = 5000000
const DESIGN_TOKEN_WARN = 0.8

export function designPhase(ms, END_CONDITIONS, DESIGN, integrated) {
  // Replace a repeated integrated issue and every referenced decision before any
  // question routing or audit, including answers, fixes and later re-plans. A
  // caller may omit these entries: the source ledger supplies them in either case.
  function pinIntegrated(call) {
    const fixed = (integrated || []).filter((i) => i.reused)
    if (!fixed.length) return call
    const design = Object.assign({}, call.output)
    const issues = new Map(fixed.map((i) => [i.node, i.issue]))
    const decisions = new Map()
    for (const i of fixed) for (const d of i.decisions) decisions.set(d.id, d)
    const seen = new Set()
    design.issues = design.issues.filter((i) => {
      if (!issues.has(i.node)) return true
      if (seen.has(i.node)) return false
      seen.add(i.node)
      return true
    }).map((i) => issues.get(i.node) || i)
    for (const [node, issue] of issues) if (!seen.has(node)) design.issues.push(issue)
    design.decisions = design.decisions.filter((d) => !decisions.has(d.id)).concat(Array.from(decisions.values()))
    return Object.assign({}, call, { output: design })
  }
  // The design audit of a DESIGN of the architect: its open questions go to explorers or, when out of the architect's authority or
  // beyond the explorer cap, to the orchestrator (crw.exploreRoute), the critic (a role of its own, in a separate review execution from the
  // architect, CRW-1452) audits the DESIGN with the answers, after the answers an open question got have gone back to the architect on
  // its own thread (design-answers, CRW-1466), and needs_changes sends it back to the architect on its own thread, for
  // AUDIT_ROUNDS rounds. From the second round on the critic's packet carries the findings of the earlier rounds by id (r<round>.<index>)
  // and the architect's disposition of each, from the DESIGN its design-fix answered (PRIOR FINDINGS). tag names
  // the labels of a re-plan's rounds. Every call of the loop reads one head: a re-plan's context names its pinned head, and designHead
  // is the head the first DESIGN's phase sampled (crw.designHead), which the first DESIGN's loop reads with no re-plan context. The answer is {ok, arch, answers}, or {ok: false, result} with what the milestone came to when
  // the design could not be approved.
  async function auditedDesign(first, tag, context, designHead) {
    let arch = pinIntegrated(first)
    // CRW-1681: thread is the CallResult agent.call returned last, the only value a resume takes. arch is that call with the reused
    // nodes' issues and decisions pinned, which the audit, the ledger and the answer read.
    let thread = first
    // the head every call of the loop reads: a re-plan's pinned head, or the design head the first DESIGN's phase sampled
    // (crw.designHead), so the open questions' explorers, the critic and the architect's turns judge the code the DESIGN was written for
    const at = (context && context.head) || designHead || undefined
    // Count uncached input plus output from meta.tokens (crw.explore keeps the same split for its explorers).
    // Cached input stays in the usage records and the detail, outside this cap. budget.run fixes the cap across resumes.
    const cap = budget.run === null ? DESIGN_TOKEN_CAP : Math.min(DESIGN_TOKEN_CAP, budget.run * DESIGN_TOKEN_WARN)
    const capText = budget.run === null ? 'the cap of DESIGN_TOKEN_CAP ' + DESIGN_TOKEN_CAP + ' tokens (the run has no budget)'
      : 'the cap of ' + cap + ' tokens (DESIGN_TOKEN_CAP ' + DESIGN_TOKEN_CAP + ', DESIGN_TOKEN_WARN ' + DESIGN_TOKEN_WARN + ' of the run budget of ' + budget.run + ')'
    let used = 0
    let cachedInput = 0
    let warned = false
    // the findings of the rounds so far, by id, each round with the dispositions the architect's design-fix answer gave (null when it
    // gave none): the critic's PRIOR FINDINGS
    const prior = []
    async function tokenCap(tokens, round) {
      const usage = tokens || {}
      const cached = usage.cached_input || 0
      used += Math.max(0, (usage.input || 0) - cached) + (usage.output || 0)
      cachedInput += cached
      if (warned || used < cap) return
      warned = true
      const detail = 'tokens: the design loop ' + (tag ? tag + ' ' : 'of the milestone ') + 'used ' + used + ' tokens by audit round ' + round + ' (uncached input + output; cached input ' + cachedInput + ' excluded), ' + capText
      log('warn design_loop_cap ' + detail)
      await crw.escalate(null, { reason: 'design_loop_tokens', detail })
    }
    const tokensOf = (call) => (call && call.meta && call.meta.tokens) || {}
    await tokenCap(tokensOf(arch), 0)
    for (let round = 1; round <= AUDIT_ROUNDS; round++) {
      // the end condition commands of the issues are run once, by the first explorer of the first DESIGN's first round (CRW-1382)
      const ends = round === 1 && tag === '' ? END_CONDITIONS : []
      // a re-plan's explorers read the head it judges (CRW-1458); the first DESIGN's context has no head key, so its bytes are as before
      // the first DESIGN's open questions keep the reuse key of their text (no head in the route's context): every answer of its loop reads
      // the same design head; their explorers are placed at it (opts.head)
      const asked = await crw.explore('open_questions', arch.output.open_questions, context && context.head ? { milestone: ms.id, head: context.head } : { milestone: ms.id }, { endConditions: ends, head: at })
      await tokenCap(asked.token_usage, round)
      // the authority questions of the round are asked now, approved or not (CRW-1366): an answer is the next round's
      await askOrchestrator(asked.answers, 'out_of_authority', asked.carried)
      if (asked.failed) return { ok: false, result: await crw.milestoneHalt(ms, 'explorer', asked.failed.status, asked.failed.reason, asked.failed.callId) }
      const answers = asked.answers
      // the answers go back to the architect on its own thread before the critic reads the DESIGN (CRW-1466): when a question of the
      // round got an answer, design-answers resumes the architect with them, and the DESIGN it answers with is the one audited (and the
      // handle a design-fix and the ledger resume); a question it raises there is routed in the next round
      if (answeredQuestions(answers).length) {
        thread = await agent.call(designAnswersPacket(answers, context), { resume: thread, schema: DESIGN, head: at, label: 'design-answers:' + tag + 'r' + round })
        if (thread.status !== 'completed') return { ok: false, result: await crw.milestoneHalt(ms, 'architect', thread.status, thread.reason, thread.callId) }
        arch = pinIntegrated(thread)
        await tokenCap(tokensOf(arch), round)
      }
      const cand = { design: arch.output, answers }
      await crw.spawnDecision('AUDIT', 'spawn', { role: 'critic', inputs: { milestone: ms.id, round, tag } })
      // the critic of a re-plan audits at the head the re-plan judges, in a read-only checkout of it, as the architect's turns are (CRW-1458)
      let call = await agent.call(designAuditPacket(ms, cand, round, prior, context, integrated), { agentType: 'critic', schema: DESIGN_AUDIT, head: at, label: 'design-audit:' + tag + 'r' + round })
      let audit = await crw.designAudit(ms.id, round, cand, arch, call)
      if (audit.state === 'null_model') {
        await tokenCap(tokensOf(call), round)
        call = await agent.call(designAuditPacket(ms, cand, round, prior, context, integrated), { agentType: 'critic', schema: DESIGN_AUDIT, head: at, label: 'design-audit:' + tag + 'r' + round + ':retry' })
        audit = await crw.designAudit(ms.id, round, cand, arch, call)
      }
      await tokenCap(tokensOf(call), round)
      if (audit.state === 'approved') return { ok: true, arch, answers, carried: asked.carried }
      if (audit.state !== 'needs_changes') return { ok: false, result: await crw.milestoneHalt(ms, 'design_audit', audit.state, audit.reason, call.callId) }
      // back to the architect, on its own thread: the DESIGN it revises is the one it wrote, and it disposes of each finding by its id
      thread = await agent.call(designFixPacket(audit, context, round), { resume: thread, schema: DESIGN, head: at, label: 'design-fix:' + tag + 'r' + round })
      if (thread.status !== 'completed') return { ok: false, result: await crw.milestoneHalt(ms, 'architect', thread.status, thread.reason, thread.callId) }
      arch = pinIntegrated(thread)
      const dispositions = arch.output.dispositions
      prior.push({ round, findings: auditFindings(round, audit.findings), dispositions: dispositions === undefined ? null : dispositions })
      await tokenCap(tokensOf(arch), round)
    }
    log('warn design_loop_cap rounds: the DESIGN ' + (tag ? tag + ' ' : 'of the milestone ') + 'was not approved in AUDIT_ROUNDS ' + AUDIT_ROUNDS + ' audit rounds')
    const esc = await crw.escalate(null, { reason: 'design_audit_rounds', detail: 'the DESIGN was not approved in ' + AUDIT_ROUNDS + ' audit rounds' })
    return { ok: false, result: { state: 'escalated', source: 'design_audit', reason: 'design_audit_rounds', escalation: esc.id } }
  }

  // The open questions routed to the orchestrator for reason are design questions for the orchestrator thread, each with its scope:
  // one non-blocking ask, and the milestone goes on. The ones out of the architect's authority are asked in the audit round that
  // routed them, the ones beyond the explorer cap of the milestone when the DESIGN is approved. The evidence names the digests asked:
  // a question the run asked already is routed asked_in_run (crw.exploreRoute) and raises no ask of its own. Only the latest design
  // ask of a run is current (the one crw flow answer accepts and the digest lists as waiting), so an ask carries, besides the
  // questions it raises, every question of this DESIGN the run asked already and no answer has reached: asked_in_run, and the
  // authority questions of the round when the approval asks the ones beyond the cap; and every question the run asked that no answer has
  // reached and this DESIGN no longer raises (carried, from crw.exploreRoute, CRW-1381), so it stays answerable through the current ask.
  async function askOrchestrator(answers, reason, carried) {
    const pending = (a) => a.route === 'orchestrator' && (a.reason === reason || a.reason === 'asked_in_run' || a.reason === 'out_of_authority')
    if (!answers.some((a) => a.route === 'orchestrator' && a.reason === reason)) return
    const asked = answers.filter(pending).concat(carried || [])
    await crw.escalate(null, { reason: 'design_question', detail: asked.map((a) => (a.scope ? '[' + a.scope + '] ' : '') + a.question).join(' | '), ask: 'design',
      evidence: { questions: asked.map((a) => a.digest) } })
  }

  // A re-plan (SP-ARCH-2): the architect's whole DESIGN of the milestone (CRW-1462: every decision and every issue of every node, the
  // started ones as they were released, not a delta) is the next turn of its thread (the handle the ledger holds), asked at the head
  // H in a read-only checkout of its own; it is audited again like the first DESIGN (audit rounds, and a return to the architect on the
  // same thread for needs_changes), and only an approved DESIGN becomes the next revision of the ledger - the nodes that have not
  // started are released under a revision only after that. signal is the number of the signal that made the re-plan (0 at the close);
  // unstarted is the ids of the nodes that have not started (none at the close), which the architect and the critic both read.
  async function replan(tag, signal, due, head, misaligned, unstarted) {
    const cur = await crw.currentDesign(ms.id)
    await crw.spawnDecision('ARCH_2', 'spawn', { role: 'architect', reason: due.reasons.join(','), inputs: {
      milestone: ms.id, revision: cur.revision, signal, head, reasons: due.reasons, plan_mismatch: due.plan_mismatch, slice_change: due.slice_change.changed } })
    // a re-plan after an imported DESIGN starts a fresh local architect on the reused design's context (CRW-1329): the foreign
    // run's thread handle is never resumed
    const call = await agent.call(replanPacket(ms, cur, due, head, misaligned, unstarted), Object.assign({ schema: DESIGN, head, label: 'replan:' + tag }, cur.handle ? { resume: cur.handle } : { agentType: 'architect' }))
    if (call.status !== 'completed') return { state: 'halted', result: await crw.milestoneHalt(ms, 'replan', call.status, call.reason, call.callId) }
    // the plans the started nodes were released with: the critic compares the whole DESIGN with them (RELEASED PLANS)
    const released = await crw.releasedPlans(ms.id)
    const audited = await auditedDesign(call, 'replan-' + tag + ':', { head, base_tip: cur.base_tip, landed: cur.landed, unstarted, released })
    if (!audited.ok) return { state: 'halted', result: audited.result }
    const led = await crw.designLedger(ms.id, { design: audited.arch.output, answers: audited.answers, architect: audited.arch, signal, head, plan: due.plan })
    await askOrchestrator(audited.answers, 'explorer_cap', audited.carried)
    return { state: 'revised', revision: led.revision }
  }

  // What a signal of the dag comes to: the numbers of SP-ARCH-2 are counted and, when a reason stands and a node has not started, the
  // milestone re-plans; the signal is then a refresh (the nodes that have not started are released under the new revision) or, when the
  // re-plan could not be done, a halt. Every decision, the ones to skip included, is a row of flow_spawn_decisions.
  async function onSignal(sig) {
    const due = await crw.replanSignals(ms.id, { phase: 'signal', signal: sig.k })
    const inputs = { milestone: ms.id, signal: sig.k, revision: due.revision, plan_mismatch: due.plan_mismatch, slice_change: due.slice_change.changed, unstarted: sig.unstarted }
    if (!due.due) {
      await crw.spawnDecision('ARCH_2', 'skip', { reason: 'no_replan_signal', inputs })
      return 'continue'
    }
    if (sig.unstarted.length === 0) {
      // nothing is left to release under another revision: the close judges the same numbers
      await crw.spawnDecision('ARCH_2', 'skip', { reason: 'no_unstarted_node', inputs: Object.assign({ reasons: due.reasons }, inputs) })
      return 'continue'
    }
    // the re-plan names the nodes the signal found unstarted: the ones its revision releases
    const out = await replan('s' + sig.k, sig.k, due, sig.tip, null, sig.unstarted)
    if (out.state !== 'revised') return { result: 'halt', reasons: due.reasons }
    // the nodes that have not started are admitted by the regions of the revision just made, not of the first one
    const cur = await crw.currentDesign(ms.id)
    const regions = {}
    for (const is of cur.design.issues) {
      const planned = ms.nodes.find((n) => n.id === is.node)
      regions[is.node] = await crw.admissionRegions(planned ? (planned.planned_regions || planned.regions) : [], is.edit_regions)
    }
    return { result: 'refresh', revision: out.revision, reasons: due.reasons, regions }
  }
  return { auditedDesign, askOrchestrator, replan, onSignal }
}

// The background questions (CRW-1466, SP-EXPL-1): before the architect's first DESIGN, one question for each node of the milestone goes
// to an explorer, which reads what the repository holds for it today; the architect's packet carries the answers (BACKGROUND
// FINDINGS). A question names the node's id, its issue key and its title, so its digest is the node's and never an open question's;
// its files_needed are the node's declared regions (none declared: not named). crw.exploreRoute spawns each or reuses an answer of the
// same digest the run has, never inline, never to the orchestrator and outside the open questions' cap. Only the first DESIGN has this
// phase: a re-plan is the next turn of the architect's thread, which has read them. The end condition commands stay with the first
// open-question explorer of the design loop.
export function backgroundQuestion(n) {
  return 'Background of node ' + n.id + ' (' + n.issue + '): ' + n.title + '. What does the repository hold today that the plan of this issue rests on: the files, types, functions and tests it will change or read, the conventions they follow, and what in them must not change?'
}
