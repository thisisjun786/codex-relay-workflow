export const meta = { name: 't1-step-from-other-path', phases: ['retry'] }
const step = await crw.tierStep('node-1', { evidence: ['two failing heads'] })
// The decision was made on the root path; this call runs on a path of its own.
const out = await parallel([async () => agent.call('retry', { model: 'sonnet', stepDecision: step.id, label: 'retry' })])
return out.length
