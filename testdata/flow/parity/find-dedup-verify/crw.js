export const meta = { name: 'find-dedup-verify', description: 'Find defects per area, drop duplicates, verify each one with a second model', phases: [{ title: 'Find' }, { title: 'Verify' }] }
const FINDINGS = { type: 'object', properties: { findings: { type: 'array', minItems: 1, items: { type: 'object', properties: { file: { type: 'string' }, claim: { type: 'string' } }, required: ['file', 'claim'], additionalProperties: false } } }, required: ['findings'], additionalProperties: false }
const CHECK = { type: 'object', properties: { verdict: { type: 'string', enum: ['confirmed', 'refuted'] }, reason: { type: 'string' } }, required: ['verdict', 'reason'], additionalProperties: false }
const areas = ['api', 'storage', 'cli']
phase('Find')
const found = await parallel(areas.map((area) => () => agent(`List the defects you can show in the ${area} package, each with its file and claim.`, { agentType: 'explorer', model: 'haiku', schema: FINDINGS, label: `find:${area}` })))
const seen = new Set()
const unique = []
for (const result of found) {
  for (const f of (result ? result.findings : [])) {
    const key = `${f.file}|${f.claim}`
    if (!seen.has(key)) { seen.add(key); unique.push(f) }
  }
}
phase('Verify')
const checks = await parallel(unique.map((f, i) => () => agent(`Check this claim against the code: ${f.claim} (${f.file})`, { model: 'ocx-gpt-6-1-sol', schema: CHECK, label: `check:${i}` })))
return { found: found.length, unique: unique.length, confirmed: checks.filter((c) => c && c.verdict === 'confirmed').length }
