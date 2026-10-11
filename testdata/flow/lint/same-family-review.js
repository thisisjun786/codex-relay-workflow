export const meta = { name: 'same-family-review', phases: ['build', 'review'] }
const built = await agent.call('build the change', { agentType: 'executor', label: 'build' })
// A review by the family that wrote the change is not independent.
const review = await agent.call('review the change', { agentType: 'executor', label: 'review:build' })
return { built: built.status, review: review.status }
