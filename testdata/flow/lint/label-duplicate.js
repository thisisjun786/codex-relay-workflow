export const meta = { name: 'label-duplicate', phases: ['scan'] }
const first = await agent.call('scan the first directory', { agentType: 'explorer', label: 'scan' })
const second = await agent.call('scan the second directory', { agentType: 'explorer', label: 'scan' })
return [first.status, second.status]
