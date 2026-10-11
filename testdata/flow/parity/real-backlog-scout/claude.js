export const meta = {
  name: 'crw-backlog-scout',
  description: 'Scout open CRW Linear issues for parallel-ready work: status, deps, file areas, size, holds',
  phases: [{ title: 'Scout' }, { title: 'Check' }],
}
const ROW = { type: 'object', properties: {
  key: { type: 'string' }, title: { type: 'string' }, status: { type: 'string' }, priority: { type: 'string' },
  depsOpen: { type: 'array', items: { type: 'string' }, description: 'blocking/predecessor issue keys that are not Done/Canceled/Duplicate' },
  areas: { type: 'array', items: { type: 'string' }, description: 'main repo paths/packages it edits (best estimate from the issue text)' },
  size: { type: 'string', enum: ['S', 'M', 'L'] },
  hold: { type: 'string', description: 'empty, or why it must wait: install/deploy, hook activation, operational DB change, risk acceptance, Jun decision, frozen, needs real Codex/installed runtime' },
  existing: { type: 'string', description: 'existing branch/worktree/state dir/pre-eval record if any' },
  ready: { type: 'boolean', description: 'true when no open deps and no hold' },
  note: { type: 'string' } },
  required: ['key', 'title', 'status', 'priority', 'depsOpen', 'areas', 'size', 'hold', 'existing', 'ready', 'note'] }
const SCHEMA = { type: 'object', properties: { rows: { type: 'array', items: ROW }, otherOpen: { type: 'array', items: { type: 'string' }, description: 'other open CRW issue keys seen (Todo/In Progress/Backlog, High or Urgent) not in the candidate list, with one-line titles' } }, required: ['rows', 'otherOpen'] }
const CANDIDATES = ['CRW-875','CRW-910','CRW-917','CRW-900','CRW-711','CRW-564','CRW-192','CRW-195','CRW-392','CRW-203','CRW-201','CRW-202','CRW-204','CRW-161','CRW-549','CRW-1051','CRW-550','CRW-1056','CRW-1057','CRW-1058','CRW-1059']
const half = [CANDIDATES.slice(0, 11), CANDIDATES.slice(11)]
const ctx = `Repository codex-relay-workflow (shared checkout /home/jun/code/codex-relay-workflow, read-only for you; run git fetch -q origin first and use origin/dev). Worktrees under /home/jun/code-worktrees/codex-relay-workflow/, state dirs /state/crw/crw-<N>/, evaluation records /state/crw/mgmt/pre-eval/crw<N>-*.json. Linear team CRW (use the Linear MCP tools via ToolSearch: get_issue with includeRelations, list_comments). Read-only task: no writes anywhere (no Linear writes, no git writes). Context: the shell reader redesign CRW-1028 is Done and in dev (09e5c5bcc); CRW-941 is being integrated; CRW-894, 895, 951, 851 are in progress on branches; CRW-1003 is frozen; installation/deployment, hook activation, operational DB changes and risk acceptance are on hold (Jun). The handoff /state/crw/mgmt/cc-handoff-20261009.md section '작업별 상태와 다음 행동' item 6-7 gives the planned order; /state/crw/mgmt/activation-readiness-shell-reader-20261009.md lists what the 10-09 re-check found still open for 875, 910, 917, 900.`
phase('Scout')
const parts = await parallel(half.map((keys, i) => () => agent(`${ctx}\nFor each of these Linear issues report one row: ${keys.join(', ')}. For depsOpen read the issue relations (blockedBy) and any predecessor keys named in the description, and check each one's current Linear status. For areas read the issue text and, if a branch exists, git diff --stat origin/dev...<branch>. ${i === 0 ? 'Also list (otherOpen) up to 25 other open High/Urgent CRW issues in the project "CXC 실행 체계를 CRW Go에 자체 이식" not in this list (use list_issues filtered by team CRW and state).' : 'otherOpen may be empty.'}`, { label: `scout:${i}`, phase: 'Scout', model: 'haiku', effort: 'high', schema: SCHEMA })))
const rows = parts.filter(Boolean).flatMap(p => p.rows)
const other = parts.filter(Boolean).flatMap(p => p.otherOpen)
phase('Check')
const CHECK = { type: 'object', properties: { corrections: { type: 'array', items: { type: 'object', properties: { key: { type: 'string' }, field: { type: 'string' }, was: { type: 'string' }, correct: { type: 'string' }, evidence: { type: 'string' } }, required: ['key', 'field', 'was', 'correct', 'evidence'] } }, summary: { type: 'string' } }, required: ['corrections', 'summary'] }
const check = await agent(`${ctx}\nYour ONLY task: check this scouting table against Linear and git and correct wrong facts (status, depsOpen, ready, hold, existing). Spot-check every row whose ready is true and every row whose depsOpen is non-empty. Return corrections with evidence.\n${JSON.stringify(rows)}`, { label: 'check', phase: 'Check', agentType: 'ocx-gpt-6-1-sol', effort: 'high', schema: CHECK })
return { rows, other, check }