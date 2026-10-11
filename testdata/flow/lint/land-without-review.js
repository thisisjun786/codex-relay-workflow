export const meta = { name: 'land-without-review', phases: ['build'] }
const node = args.node
const ex = await agent.call('build', { agentType: 'executor', node, label: 'build' })
const head = await crw.commit(node, ex)
await crw.verify(node, head)
return crw.land(node, { head })
