export const meta = { name: 'order-review-before-verify', phases: ['build'] }
const node = args.node
const ex = await agent.call('build', { agentType: 'executor', node, label: 'build' })
const head = await crw.commit(node, ex)
await crw.review(node, head, null)
return crw.verify(node, head)
