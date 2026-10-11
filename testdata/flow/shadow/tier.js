import { RECEIPT, VERDICT } from 'crw:lib/schemas'
export const meta = { name: 'scenario-tier' }
// n3 is built on the higher tier at once, with the step decision of n2
phase('n1')
await agent.call('build n1', { agentType: 'executor', node: 'n1', label: 'build-n1', schema: RECEIPT })
await agent.call('review n1', { agentType: 'reviewer', node: 'n1', label: 'review-n1', schema: VERDICT })
phase('n2')
await agent.call('build n2', { agentType: 'executor', node: 'n2', label: 'build-n2' })
const step = await crw.tierStep('n2', { why: 'two failed heads' })
await agent.call('build n2 again', { agentType: 'executor-t1', node: 'n2', label: 'build-n2-t1', stepDecision: step.id })
phase('n3')
await agent.call('build n3', { agentType: 'executor-t1', node: 'n3', label: 'build-n3', stepDecision: step.id })
return 'done'
