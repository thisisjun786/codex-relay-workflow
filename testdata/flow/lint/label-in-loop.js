export const meta = { name: 'label-in-loop', phases: ['find'] }
const found = []
for (let round = 0; round < 3; round++) {
  found.push(await agent.call(`find round ${round}`, { agentType: 'explorer', label: 'find' }))
}
return found.length
