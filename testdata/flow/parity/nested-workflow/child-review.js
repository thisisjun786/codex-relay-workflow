export const meta = { name: 'child-review', description: 'A design review and a second-model check of a summary', phases: [{ title: 'Review' }] }
const OPINION = { type: 'object', properties: { opinion: { type: 'string' } }, required: ['opinion'], additionalProperties: false }
const CHECK = { type: 'object', properties: { verdict: { type: 'string', enum: ['verified', 'needs_changes'] } }, required: ['verdict'], additionalProperties: false }
const design = await agent(`Review the design of this change: ${args.summary}`, { model: 'opus', schema: OPINION, label: 'review:design' })
const check = await agent(`Check this opinion against the change: ${design.opinion}`, { agentType: 'ocx-gpt-6-1-sol', effort: 'high', schema: CHECK, label: 'review:check' })
return { design: design.opinion, check: check.verdict }
