export const meta = { name: 'ambiguous-haiku', phases: ['build'] }
const built = await agent.call('build the thing', { model: 'haiku', effort: 'medium', label: 'build' })
return built.status
