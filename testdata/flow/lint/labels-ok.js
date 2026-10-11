export const meta = { name: 'labels-ok', phases: ['find'] }
// Alternative branches may share a label, a template varies it per round, and each item of a join is its own path.
const mode = args && args.deep ? 'deep' : 'quick'
if (mode === 'deep') {
  await agent.call('scan deeply', { agentType: 'explorer', label: 'scan' })
} else {
  await agent.call('scan quickly', { agentType: 'explorer', label: 'scan' })
}
for (let round = 0; round < 2; round++) {
  await agent.call(`find ${round}`, { agentType: 'explorer', label: `find:${round}` })
}
const items = ['a', 'b']
const out = await parallel(items.map((x) => async () => agent(`look at ${x}`, { agentType: 'explorer', label: 'look' })))
return out.length
