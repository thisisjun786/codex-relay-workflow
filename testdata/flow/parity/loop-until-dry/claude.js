export const meta = { name: 'loop-until-dry', description: 'Ask for new issues round after round until a round finds nothing new', phases: [{ title: 'Sweep' }] }
const ISSUES = { type: 'object', properties: { issues: { type: 'array', minItems: 1, items: { type: 'string' } } }, required: ['issues'], additionalProperties: false }
const known = new Set()
phase('Sweep')
let rounds = 0
for (let round = 1; round <= 4; round++) {
  const out = await agent(`Round ${round}: list issues not in this list: ${[...known].join(', ') || 'none yet'}.`, { model: 'haiku', schema: ISSUES, label: `sweep:round${round}` })
  rounds = round
  const fresh = (out ? out.issues : []).filter((issue) => !known.has(issue))
  if (fresh.length === 0) break
  for (const issue of fresh) known.add(issue)
}
return { rounds, known: [...known] }
