export const meta = { name: 'nested-workflow', description: 'Build a change, then hand it to a nested review workflow', phases: [{ title: 'Build' }, { title: 'Review' }] }
const RECEIPT = { type: 'object', properties: { summary: { type: 'string' } }, required: ['summary'], additionalProperties: false }
phase('Build')
const built = await agent('Implement the change in the working directory and summarise it.', { model: 'haiku', schema: RECEIPT, label: 'build:main' })
phase('Review')
const review = await workflow('child-review', { summary: built.summary })
return { built: built.summary, review }
