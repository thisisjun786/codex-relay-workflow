export const meta = {
  name: 'routing-smoke-test',
  description: 'Check that workflow agents route to haiku (build) and gpt-6.1-sol (verify)',
  phases: [{ title: 'Build' }, { title: 'Verify' }],
}
const r = await pipeline(
  [1],
  () => agent('Routing test. Use no tools. Return one line: your model name and "WF-BUILD-5521".', { label: 'build:haiku', phase: 'Build', model: 'haiku', effort: 'medium' }),
  (prev) => agent(`Routing test. Use no tools. The build step returned: "${prev}". Return one line: your model id and "WF-VERIFY-5521".`, { label: 'verify:sol', phase: 'Verify', agentType: 'ocx-gpt-6-1-sol', model: 'haiku', effort: 'high' }),
)
return r