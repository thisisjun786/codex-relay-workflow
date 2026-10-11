export const meta = { name: 't1-without-step-decision', phases: ['retry'] }
const retry = await agent.call('build it again on the second tier', { model: 'sonnet', label: 'retry' })
return retry.status
