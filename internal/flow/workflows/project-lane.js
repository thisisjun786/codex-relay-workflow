import { RECEIPT, DESIGN, DESIGN_AUDIT, REFLECTION } from 'crw:lib/schemas'
import { install } from 'crw:lib/lane'
import { reportClose } from 'crw:lib/close'
import { answeredQuestions, architectPacket, auditFindings, designAnswersPacket, designAuditPacket, designFixPacket, reflectionPacket, replanPacket, packet, answerPacket, fixPacket, subTaskPacket } from 'crw:lib/packets'
import { designPhase, backgroundQuestion } from 'crw:lib/project-design'
import { EPOCH_LOST, epochLost, lane } from 'crw:lib/project-build'
import { milestonePhase } from 'crw:lib/project-close'
export const meta = { name: 'project-lane', description: 'Run the issues of a milestone: background explorers, architect, design audit by the critic, explorers, one lane per issue node, milestone close and report', phases: ['explore', 'design', 'build', 'milestone'], args: { plan: 'string', milestone: 'string', cap: 'number' } }

// The project lane. Explorers first read the repository for each node of the milestone (the background questions, CRW-1466), the
// milestone is designed once by its architect over what they found, the DESIGN is audited by the critic in a separate review execution,
// before any executor starts, every issue node then runs its own lane (an executor, the unit check of its uncommitted work by a
// separately invoked review tool, run in Go with no reviewer agent or agent thread id, the run's commit and verification, the land). An observed same-thread review is warning-only and recorded, and the run continues; model family is provenance. The
// milestone is closed over what the lanes integrated. Every fact a lane uses comes from the journal: the plan of its issue from the
// design revision it was released under (crw.issuePlan), the evidence of its tier from the calls and checks it made. The operations
// are the host's (internal/flow/ops); the composites (crw.dag, crw.land, ...) come from crw:lib/lane.
install(crw)

const ms = await crw.milestone(args.plan, args.milestone)
// the commands of the issues' done conditions (CRW-1382): the design-stage explorer runs each once and records the exit code and the tests
const END_CONDITIONS = []
for (const n of ms.nodes) for (const c of n.end_condition || []) END_CONDITIONS.push({ issue: n.issue, command: c })

// the DESIGN of an earlier run of this plan and repository is reused when crw.designReuse finds it approved over the same slices
// and milestone digest (CRW-1329): the run's design phase is skipped and its ledger's first revision imports it
const reused = await crw.designReuse(ms.id)
// The same read records proven integrations before any design call. Their source
// issues and decisions are fixed; only the remaining nodes need fresh design.
const integrated = reused.integrated_design || []
const designNodes = ms.nodes.filter((n) => !integrated.some((i) => i.reused && i.node === n.id))
const { auditedDesign, askOrchestrator, replan, onSignal } = designPhase(ms,
  END_CONDITIONS.filter((c) => designNodes.some((n) => n.issue === c.issue)), DESIGN, integrated)
let initialDesign
if (reused.reused) {
  phase('design')
  await crw.spawnDecision('ARCH_1', 'skip', { role: 'architect', reason: 'approved_design_reused', inputs: { milestone: ms.id, source_run: reused.source_run, source_revision: reused.source_revision, digest: reused.digest } })
  await crw.designLedger(ms.id, { reuse: true })
  initialDesign = reused.design
} else {
  phase('explore')
  // one design head for the phase (crw.designHead): every background explorer and the architect's first call read it, whatever lands on
  // the integration target meanwhile, and the architect's checkout, and so the head r0 records the DESIGN ran at, is made at it
  const designAt = await crw.designHead(ms.id)
  const background = await crw.explore('background', designNodes.map((n) => ({ id: 'bg-' + n.id, question: backgroundQuestion(n), scope: '', files_needed: n.regions.length ? n.regions : null })), { milestone: ms.id, head: designAt.head }, {})
  if (background.failed) return crw.milestoneHalt(ms, 'explorer', background.failed.status, background.failed.reason, background.failed.callId)

  phase('design')
  await crw.spawnDecision('ARCH_1', 'spawn', { role: 'architect', inputs: { milestone: ms.id, nodes: ms.nodes.length } })
  const first = await agent.call(architectPacket(ms, background.answers, designAt.head, integrated), { agentType: 'architect', schema: DESIGN, head: designAt.head, label: 'architect' })
  if (first.status !== 'completed') return crw.milestoneHalt(ms, 'architect', first.status, first.reason, first.callId)
  const designed0 = await auditedDesign(first, '', undefined, designAt.head)
  if (!designed0.ok) return designed0.result
  const arch = designed0.arch
  await crw.designLedger(ms.id, { design: arch.output, answers: designed0.answers, architect: arch })
  await askOrchestrator(designed0.answers, 'explorer_cap', designed0.carried)
  initialDesign = arch.output
}

phase('build')
// A node is admitted by the regions of the plan and of the approved DESIGN together (crw.admissionRegions, the Go op): the executor of a
// node edits the DESIGN's edit_regions, so the nodes that run at once must not overlap in those either. A planned region keeps the plan's
// change and hold (planned_regions, when the plan states one that is not a plain edit), which crw.regionsOverlap judges by (CRW-1461).
const designed = {}
for (const is of initialDesign.issues) designed[is.node] = is.edit_regions
// A node the relay already reads as integrated is not built again (CRW-1597): before the lanes are admitted,
// crw.reuseIntegrated proves the earlier run's integration of it - the relay's current acceptance and integration observations,
// the unchanged node slice and approved DESIGN, and the source run's acceptance, integration and audit records against the
// current target. A proven node joins the lanes as landed with its source provenance; a pending node runs its ordinary lane.
// The integrated proof comes from the relay and the earlier run's journal; the final close evidence (gate, crosscheck, report)
// is this run's own, made at the close below - no final-gate evidence is copied between runs.
const inherited = []
const pending = []
for (const n of ms.nodes) {
  const saved = await crw.reuseIntegrated(ms.id, n.id)
  if (saved.reused) inherited.push({ node: n.id, state: 'landed', head: saved.head, batch_commit: saved.batch_commit, acceptance_id: saved.acceptance_id, source_run: saved.source_run, reused: true })
  else pending.push(n)
}
const admitted = []
for (const n of pending) admitted.push(Object.assign({}, n, { regions: await crw.admissionRegions(n.planned_regions || n.regions, designed[n.id]) }))
const fresh = admitted.length ? await crw.dag(args.plan, lane, { nodes: admitted, milestone: ms.id, cap: args.cap, onSignal }) : []
const completed = inherited.concat(fresh)
const lanes = ms.nodes.map((n) => completed.find((l) => l && l.node === n.id))
const lost = lanes.filter((l) => l && l.reason === EPOCH_LOST)
if (lost.length > 0) return { state: 'stopped', reason: EPOCH_LOST, detail: (lost.find((l) => l.detail) || lost[0]).detail || '', milestone: ms.id, lanes }
// the run-level hold plan_superseded (CRW-1291): the relay's plan moved past the revision the run pinned, and the run holds for its parent
if (lanes.some((l) => l && l.state === 'held' && l.reason === 'plan_superseded')) return { state: 'held', reason: 'plan_superseded', milestone: ms.id, lanes }
if (!lanes.every((l) => l && l.state === 'landed')) return { state: 'incomplete', milestone: ms.id, lanes }

// A fenced write the steps after the lanes make (the close and the re-plan) once another session claimed the plan's epoch ends the run
// stopped with the reason too (CRW-1382), as a lane that meets it does: closeAndReport() runs those steps and the catch below takes the error kind.
const closeAndReport = milestonePhase(ms, lanes, replan)
try {
  const closed = await closeAndReport()
  if (closed.state !== 'closed') return closed
  // the milestone closed: the parent's pull request of the bundle (a no-op for a direct landing), then its merge request (CRW-1423)
  const opened = await crw.prOpen(ms.id)
  if (opened.state === 'not_required') return closed
  if (opened.state !== 'opened') return Object.assign({}, closed, { state: 'held', reason: opened.reason, pr: opened.pr })
  const requested = await crw.mergeRequest(ms.id, { pr: opened.pr, review: { record: opened.review_record } })
  if (requested.state !== 'requested') return Object.assign({}, closed, { state: 'held', reason: requested.reason, pr: opened.pr, merge_request: requested })
  return Object.assign({}, closed, { pr: opened.pr, merge_request: requested })
} catch (e) {
  if (epochLost(e)) return { state: 'stopped', reason: EPOCH_LOST, detail: e.message, milestone: ms.id, lanes }
  throw e
}
