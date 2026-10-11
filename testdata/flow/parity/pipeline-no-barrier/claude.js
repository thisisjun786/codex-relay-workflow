export const meta = { name: 'pipeline-no-barrier', description: 'Each file goes through note, apply and verify without waiting for the others', phases: [{ title: 'Pipeline' }] }
const NOTES = { type: 'object', properties: { summary: { type: 'string' } }, required: ['summary'], additionalProperties: false }
const VERDICT = { type: 'object', properties: { verdict: { type: 'string', enum: ['verified', 'needs_changes'] } }, required: ['verdict'], additionalProperties: false }
const files = ['a.go', 'b.go']
phase('Pipeline')
const results = await pipeline(
  files,
  (file) => agent(`Read ${file} and plan what must change.`, { model: 'opus', schema: NOTES, label: `plan:${file}` }),
  async (notes, file) => {
    return agent(`Apply this to ${file}: ${notes.summary}`, { model: 'sonnet', schema: NOTES, label: `apply:${file}` })
  },
  (applied, file) => agent(`Verify the change to ${file}: ${applied.summary}`, { agentType: 'ocx-gpt-6-1-sol', effort: 'high', schema: VERDICT, label: `verify:${file}` }),
)
return results.map((r) => (r ? r.verdict : null))
