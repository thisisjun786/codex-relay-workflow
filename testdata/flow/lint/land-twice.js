export const meta = { name: 'land-twice', phases: ['build'] }
const node = args.node
const ex = await agent.call('build', { agentType: 'executor', node, label: 'build' })
const head = await crw.commit(node, ex)
const ci = await crw.verify(node, head)
await crw.review(node, head, ci)
await crw.land(node, { head })
return crw.land(node, { head })
