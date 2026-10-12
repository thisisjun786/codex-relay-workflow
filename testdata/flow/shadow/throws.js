export const meta = { name: 'throws' }
await agent.call('build n1', { agentType: 'executor', node: 'n1', label: 'build-n1' })
throw new Error('the script fails')
