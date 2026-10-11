export const meta = { name: 't1-with-step-decision', phases: ['retry'] }
const step = await crw.tierStep('node-1', { evidence: ['two failing heads'] })
const retry = await agent.call('build it again on the second tier', { model: 'sonnet', stepDecision: step.id, label: 'retry' })
return retry.status
