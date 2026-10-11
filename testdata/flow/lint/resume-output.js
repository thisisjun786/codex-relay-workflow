export const meta = { name: 'resume-output', phases: ['build'] }
const out = await agent('build it', { agentType: 'executor', label: 'build' })
const more = await agent.call('keep going', { resume: out, label: 'more' })
return more.status
