export const meta = { name: 'night-batch', description: 'Run the issues a night batch decision selected: the project lane over the milestone whose nodes they are, inside the decision token budget and deadline', phases: ['select', 'batch', 'digest'], args: { decision: 'string', label: 'string', issues: 'array', plan: 'string', milestone: 'string', cap: 'number', deadline: 'string', token_budget: 'number' } }

// The night batch (CRW-1205, with 1040a). The manager selected the issues and recorded the decision (crw manage batch decide);
// `crw flow start night-batch --decision D` started this run from it. Releases happen only inside the run, so there is no
// freeze and no grant: the run reads the milestone the decision names, checks that its nodes are exactly the selected issues,
// and lets the project lane run them. The budget and the deadline are the run's own (the engine pauses the calls when the
// token budget is spent, and the run process pauses the run at the deadline): a lane that is held by either is carried to the
// next night, with the reason, and the morning report says so.

phase('select')
const ms = await crw.milestone(args.plan, args.milestone)
const wanted = {}
for (const key of args.issues) wanted[key] = true
const have = {}
for (const n of ms.nodes) have[n.issue] = n.id
const missing = args.issues.filter((key) => !have[key])
const extra = ms.nodes.filter((n) => !wanted[n.issue]).map((n) => n.issue)
if (missing.length > 0 || extra.length > 0) {
  // the milestone is not the selection: nothing is released, and the manager is asked for the plan that is
  const esc = await crw.escalate(null, { reason: 'night_batch_selection', detail: 'issues the milestone lacks: ' + missing.join(', ') + '; issues it holds beyond the selection: ' + extra.join(', ') })
  return { state: 'selection_mismatch', decision: args.decision, selected: args.issues, nodes: have, missing, extra, escalation: esc.id }
}

phase('batch')
const result = await workflow('project-lane', { plan: args.plan, milestone: args.milestone, cap: args.cap })

phase('digest')
return { state: result.state === 'closed' ? 'closed' : 'incomplete', decision: args.decision, label: args.label, selected: args.issues, nodes: have, lanes: result.lanes || [], milestone: result }
