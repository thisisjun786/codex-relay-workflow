export const meta = { name: 'classify-agent-output', phases: ['build'] }
const state = crw.classify(await agent('build it', { agentType: 'executor', label: 'build' }))
return state
