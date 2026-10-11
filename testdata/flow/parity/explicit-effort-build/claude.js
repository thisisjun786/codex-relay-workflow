export const meta = { name: 'explicit-effort-build', description: 'A quick build that asks for a medium effort and a medium-effort check', phases: [{ title: 'Build' }, { title: 'Check' }] }
const RECEIPT = { type: 'object', properties: { summary: { type: 'string' } }, required: ['summary'], additionalProperties: false }
const CHECK = { type: 'object', properties: { verdict: { type: 'string', enum: ['verified', 'needs_changes'] } }, required: ['verdict'], additionalProperties: false }
phase('Build')
const built = await agent('Make the small change described in the task and summarise it.', { model: 'haiku', effort: 'medium', schema: RECEIPT, label: 'build:quick' })
phase('Check')
const check = await agent(`Check this change: ${built.summary}`, { model: 'ocx-gpt-6-1-sol', effort: 'medium', schema: CHECK, label: 'check:quick' })
return { summary: built.summary, verdict: check.verdict }
