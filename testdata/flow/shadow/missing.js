import { RECEIPT, VERDICT } from 'crw:lib/schemas'
export const meta = { name: 'scenario-missing' }
// the recorded run without the build of n3 (and so without the escalation it ended in)
phase('n1')
await agent.call('build n1', { agentType: 'executor', node: 'n1', label: 'build-n1', schema: RECEIPT })
await agent.call('review n1', { agentType: 'reviewer', node: 'n1', label: 'review-n1', schema: VERDICT })
phase('n2')
await agent.call('build n2', { agentType: 'executor', node: 'n2', label: 'build-n2' })
const step = await crw.tierStep('n2', { why: 'two failed heads' })
await agent.call('build n2 again', { agentType: 'executor-t1', node: 'n2', label: 'build-n2-t1', stepDecision: step.id })
return 'done'
