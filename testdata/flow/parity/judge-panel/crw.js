export const meta = { name: 'judge-panel', description: 'Three judges score a proposal, a final judge decides', phases: [{ title: 'Judge' }, { title: 'Decide' }] }
const SCORE = { type: 'object', properties: { score: { type: 'integer', minimum: 1, maximum: 10 }, reason: { type: 'string' } }, required: ['score', 'reason'], additionalProperties: false }
const DECISION = { type: 'object', properties: { decision: { type: 'string', enum: ['accept', 'reject'] }, reason: { type: 'string' } }, required: ['decision', 'reason'], additionalProperties: false }
const lenses = ['correctness', 'cost', 'risk']
phase('Judge')
const scores = await parallel(lenses.map((lens) => () => agent(`Score the proposal on ${lens}, 1 to 10.`, { model: 'opus', effort: 'high', schema: SCORE, label: `judge:${lens}` })))
const total = scores.reduce((sum, s) => sum + (s ? s.score : 0), 0)
phase('Decide')
const decision = await agent(`The panel scored ${total} of 30. Decide whether to accept the proposal.`, { model: 'ocx-gpt-6-1-sol', effort: 'xhigh', schema: DECISION, label: 'decide' })
return { total, decision: decision.decision }
