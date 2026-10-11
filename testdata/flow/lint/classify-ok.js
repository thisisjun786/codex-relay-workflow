export const meta = { name: 'classify-ok', phases: ['build'] }
const built = await agent.call('build it', { agentType: 'executor', label: 'build' })
return crw.classify(built) + ':' + crw.classify(built.handle)
