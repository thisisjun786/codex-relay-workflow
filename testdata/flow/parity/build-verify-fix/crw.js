export const meta = { name: 'build-verify-fix', description: 'Build a change, verify it with a second model, fix what the verifier found', phases: [{ title: 'Build' }, { title: 'Verify' }, { title: 'Fix' }] }
const RECEIPT = { type: 'object', properties: { summary: { type: 'string' } }, required: ['summary'], additionalProperties: false }
const VERDICT = { type: 'object', properties: { verdict: { type: 'string', enum: ['needs_changes', 'verified'] }, findings: { type: 'array', items: { type: 'string' } } }, required: ['verdict', 'findings'], additionalProperties: false }
phase('Build')
const built = await agent('Implement the change described in the task in the working directory and summarise what you did.', { model: 'haiku', isolation: 'worktree', schema: RECEIPT, label: 'build:change' })
phase('Verify')
const verdict = await agent(`Verify this change and list what is wrong with it: ${built.summary}`, { agentType: 'ocx-gpt-6-1-sol', effort: 'high', schema: VERDICT, label: 'verify:change' })
if (verdict.verdict === 'verified') return { verdict: verdict.verdict, fixed: false }
phase('Fix')
const fixed = await agent(`Fix these findings in the working directory: ${verdict.findings.join('; ')}`, { model: 'haiku', isolation: 'worktree', schema: RECEIPT, label: 'fix:change' })
return { verdict: verdict.verdict, fixed: true, summary: fixed.summary }
