export const meta = { name: 'ok-lifecycle', phases: ['build'] }
// One executor node: build once, rounds of commit, verify, review, land at most once.
async function lane(node) {
  let ex = await agent.call(`build ${node}`, { agentType: 'executor', node, label: 'build' })
  for (let round = 1; round < 4; round++) {
    const head = await crw.commit(node, ex)
    const ci = await crw.verify(node, head)
    const rv = ci.pass ? await crw.review(node, head, ci) : null
    if (rv && rv.verdict === 'verified') return crw.land(node, { head })
    ex = await agent.call(`fix ${node}`, { resume: ex.handle, label: `fix:${round}` })
  }
}
return await lane(args.node)
