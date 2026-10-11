export const meta = {
  name: 'crw-wave1-judge-per-lane',
  description: 'Per-lane adversarial check of wave-1 backlog lanes (one narrow GPT judge per lane; the single judge overflowed its context)',
  phases: [{ title: 'Judge' }],
}
const LANES = [
  { slug: 'zone-null-guards', keys: ['CRW-835'], files: 'internal/relay/store/dag_zone.go, internal/relay/store/dag_zone_null_key_test.go, internal/relay/store/domain_rows.go (scanAcceptanceRefresh)' },
  { slug: 'dag-correct-skip', keys: ['CRW-864'], files: 'internal/relay/dagsched (correction/Compose paths named in the issue), docs/relay' },
  { slug: 'verify-pins-stale-push', keys: ['CRW-1026'], files: 'internal/relay/dagsched and internal/relay/integrate (JudgeVerificationRecord, dag-integrate-push), docs/relay/dag-scheduler.md' },
  { slug: 'pump-signals', keys: ['CRW-692'], files: 'internal/manage/pump.go and the DagReview/capacity/audit readers it would call' },
  { slug: 'decisions-derive', keys: ['CRW-718'], files: 'internal/relay/decisions (Record, Merge, Fingerprint) and the supervisor receipt/work-report types it would read through a DTO' },
  { slug: 'managed-unload-race', keys: ['CRW-1070'], files: 'internal/relay/managed (unload/archive retry, fake adapter hooks archiveLostReply, onArchivedList)' },
]
const J = { type: 'object', properties: { lane: { type: 'string' }, approve: { type: 'boolean' }, problems: { type: 'array', items: { type: 'object', properties: { severity: { type: 'string', enum: ['blocker', 'major', 'minor'] }, problem: { type: 'string' }, fix: { type: 'string' } }, required: ['severity', 'problem', 'fix'] } }, holdForRedesign: { type: 'boolean' }, holdWhy: { type: 'string' } }, required: ['lane', 'approve', 'problems', 'holdForRedesign', 'holdWhy'] }
phase('Judge')
const out = await parallel(LANES.map(l => () => agent(`Your ONLY task: adversarially judge ONE planned lane for CRW (codex-relay-workflow, Go): lane "${l.slug}" for ${l.keys.join(', ')}. Keep your reading narrow (your context is limited): read the lane entry and the decisions for ${l.keys.join(', ')} in /state/crw/mgmt/backlog-scope/wave1.json (use jq or python to extract only those parts), the issue snapshot(s) /state/crw/crw-<n>/inputs/linear-issue-CRW-<n>.json, and only these code areas at /home/jun/code-worktrees/codex-relay-workflow/crw-triage-1010 (dev 04d73aeb0, read-only): ${l.files}. Use grep and sed -n ranges; never print whole large files.
Check: the lane notes and decisions match the issue's end conditions and the Linear design; no capability is removed; it is buildable and provable in isolation now; zone schema changes respect the rule that only CRW-835/974 may land before the cutover cut point (others integrate after it); the verification level is adequate. Also say whether the lane should wait for an upcoming redesign (repo manager > initiative parent > project child orchestrator > per-issue executor sub-agents, and a relay structure optimization that may simplify managed start/unload, delivery and subscription cleanup for long-lived project children) because that redesign would likely change or remove it. Approve only with no blocker/major problems. Do not write files or Linear.`, { label: `judge:${l.slug}`, phase: 'Judge', agentType: 'ocx-gpt-6-1-sol', effort: 'xhigh', schema: J })))
return out
