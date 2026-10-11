export const meta = {
  name: 'cxc-port-scout',
  description: 'Read-only status survey of the CXC port issues (Linear + branches + /state records) for planning',
  phases: [{ title: 'Survey', detail: 'haiku readers over issue groups' }, { title: 'Merge', detail: 'one table' }],
}

const GROUPS = [
  { key: 'g1', issues: ['CRW-932', 'CRW-900', 'CRW-910', 'CRW-895', 'CRW-706', 'CRW-704'] },
  { key: 'g2', issues: ['CRW-1005', 'CRW-1013', 'CRW-1015', 'CRW-1016', 'CRW-1017', 'CRW-1018', 'CRW-1019', 'CRW-1020', 'CRW-1047'] },
  { key: 'g3', issues: ['CRW-711', 'CRW-195', 'CRW-564', 'CRW-192', 'CRW-392'] },
  { key: 'g4', issues: ['CRW-201', 'CRW-203', 'CRW-202', 'CRW-204', 'CRW-161', 'CRW-549', 'CRW-1051', 'CRW-550'] },
]

const ROW = {
  type: 'object',
  properties: {
    rows: { type: 'array', items: { type: 'object', properties: {
      issue: { type: 'string' }, title: { type: 'string' }, linearStatus: { type: 'string' }, project: { type: 'string' },
      goal: { type: 'string', description: '1-3 sentences: what the issue delivers' },
      doneCriteria: { type: 'string', description: 'short summary of the end condition' },
      dependsOn: { type: 'array', items: { type: 'string' } },
      branch: { type: 'string', description: 'git branch name if any (codex/crw-N-*), else empty' },
      branchHead: { type: 'string' }, branchAheadOfDev: { type: 'integer', description: 'commits on branch not in origin/dev; -1 if no branch' },
      inDev: { type: 'boolean', description: 'true if the work is already merged into origin/dev' },
      worktree: { type: 'string' },
      stateDir: { type: 'string', description: '/state/crw/crw-N if it exists' },
      latestHandoff: { type: 'string', description: 'newest handoff/eval file path and its one-line status' },
      premergeEval: { type: 'string', description: 'newest /state/crw/mgmt/pre-eval/crwN-*.json or prN-*.json score and file, if any' },
      notes: { type: 'string', description: 'anything the coordinator must know (blocked, duplicate, partially done, risky)' },
    }, required: ['issue', 'title', 'linearStatus', 'goal', 'branch', 'branchAheadOfDev', 'inDev', 'notes'] } },
  },
  required: ['rows'],
}

function prompt(g) {
  return `Read-only survey for planning. Do not write anything anywhere (no Linear writes, no git changes, no files).
For each of these Linear issues: ${g.issues.join(', ')}
1. Read the issue with the Linear MCP tool get_issue (load it via ToolSearch 'select:mcp__linear__get_issue,mcp__linear__list_comments'); read the latest 3 comments with list_comments if the description does not show the current state.
2. In the git repository /home/jun/code/codex-relay-workflow (read-only): find branches for the issue: git branch -a --list '*crw-<N>*' (also remote). For each, git rev-list --count origin/dev..<branch> and whether its head is an ancestor of origin/dev (git merge-base --is-ancestor). Also git log origin/dev --oneline --grep 'CRW-<N>' | head -3 to see if it was integrated. git worktree list | grep 'crw-<N>' for a worktree.
3. Look in /state/crw/crw-<N>/ (ls -t | head) for handoff*.json, cc-progress.md, criteria.json, and in /state/crw/mgmt/pre-eval/ for crw<N>-*.json or pr*-*.json naming this issue (grep -l '"issue": "CRW-<N>"'), newest first; read the newest handoff's status line and the eval score.
Note: CRW-900 may appear as n-crw-900c (a follow-up node of CRW-900); report what you find for 900c.
Return one row per issue. Keep each text field short and factual.`
}

phase('Survey')
const parts = await parallel(GROUPS.map(g => () => agent(prompt(g), { label: `scout:${g.key}`, phase: 'Survey', model: 'haiku', effort: 'medium', schema: ROW })))
const rows = parts.filter(Boolean).flatMap(p => p.rows)
const missing = GROUPS.flatMap(g => g.issues).filter(i => !rows.some(r => r.issue === i))
if (missing.length) log(`no row for: ${missing.join(', ')}`)
return { rows, missing }