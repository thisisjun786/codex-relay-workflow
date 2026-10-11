export const meta = {
  name: 'crw-project-scout',
  description: 'Scout open CRW Linear projects for issues to connect with the CRW practical-use goal',
  phases: [{ title: 'Scout' }, { title: 'Check' }],
}
const ROW = { type: 'object', properties: {
  key: { type: 'string' }, title: { type: 'string' }, status: { type: 'string' }, milestone: { type: 'string' },
  depsOpen: { type: 'array', items: { type: 'string' } },
  areas: { type: 'array', items: { type: 'string' } },
  size: { type: 'string', enum: ['S', 'M', 'L'] },
  hold: { type: 'string', description: 'empty, or why it must wait: install/deploy, hook activation, operational DB, risk acceptance, Jun decision, frozen, external answer, real Codex runtime' },
  ready: { type: 'boolean' },
  link: { type: 'string', enum: ['critical-path', 'parallel-now', 'after-usable', 'unrelated'], description: 'critical-path: blocks CRW-392 activation or the M8 acceptance chain; parallel-now: independent work that can run now without conflicting with the shell-reader and hook-port lanes; after-usable: needs a usable CRW first; unrelated' },
  note: { type: 'string' } },
  required: ['key', 'title', 'status', 'milestone', 'depsOpen', 'areas', 'size', 'hold', 'ready', 'link', 'note'] }
const SCHEMA = { type: 'object', properties: { project: { type: 'string' }, projectStatus: { type: 'string' }, summary: { type: 'string', description: 'what the project still needs, 2-4 sentences' }, rows: { type: 'array', items: ROW } }, required: ['project', 'projectStatus', 'summary', 'rows'] }
const ctx = `Read-only task. Linear workspace (team CRW): use the Linear MCP tools via ToolSearch (list_issues with project filter and states backlog/unstarted/started, get_issue with includeRelations, list_comments). Repository codex-relay-workflow (shared checkout /home/jun/code/codex-relay-workflow, read-only; use git -C ... log origin/dev and grep to see whether something already landed). No writes anywhere.
Goal context (Jun, 10-09): bring the CXC port and CRW to practical use fast — CRW hooks switched on behind CRW-392's switch and three real trials passed (CRW-203 harness), then installation (Jun approves), then hand development back to Codex. Lanes already running: shell-reader security issues 910 917 900 1058 1059 1062 1063 1064 875 951 894; hook/CLI ports 564 192 383 195 (386 after 192); 711 fuzz campaign; small items 1056 1057 1061 992. Holds: installation/deployment, hook activation, operational DB changes, risk acceptance; CRW-1003 frozen; 549/1051/550 wait for the dots interface list.
For the project below list every open issue (Backlog, Todo, In Progress) as one row. Do not list Done/Canceled/Duplicate issues.`
const PROJECTS = [
  'P-CRW-130 "CXC 이식 가지: orchestrate·PreToolUse·doctor 보조 마무리"',
  'P-CRW-129 "관리 세션 도구 제품화와 CRW 자기 개선"',
  'P-CRW-64 "Codex 멀티스레드 DAG 배정·진척·복구"',
  'P-CRW-57 "CRW GUI: CXC GUI 이식과 전역 설정"',
  'P-CRW-131 "CRW 저장 위치: 단일 디스크 기본값과 호스트별 배치" and P-CRW-60 "Linear 기반 멀티 에이전트 클라우드 실행" and P-CRW-55 "이니셔티브 감독과 3단계 작업 운영 정식화"',
  'P-CRW-123..127 "로컬 지출 분석 도구" (five projects: CSV·집계 기반, 다중 포맷 가져오기, 로컬 API·조회 UI, 내보내기·재현 fixture, 통합·회귀 검증) — also say in summary whether this is meant as a real-use trial project for CRW/LINA-OS management',
]
phase('Scout')
const parts = await parallel(PROJECTS.map((p, i) => () => agent(`${ctx}\nProject: ${p}`, { label: `scout:${i}`, phase: 'Scout', model: 'haiku', effort: 'high', schema: SCHEMA })))
const ok = parts.filter(Boolean)
phase('Check')
const CHECK = { type: 'object', properties: { corrections: { type: 'array', items: { type: 'object', properties: { key: { type: 'string' }, field: { type: 'string' }, was: { type: 'string' }, correct: { type: 'string' }, evidence: { type: 'string' } }, required: ['key', 'field', 'was', 'correct', 'evidence'] } }, summary: { type: 'string' } }, required: ['corrections', 'summary'] }
const check = await agent(`${ctx}\nYour ONLY task: check this scouting result against Linear and git. Spot-check every row with link critical-path or parallel-now and every row with ready=true; correct wrong status, depsOpen, ready, hold or link with evidence. Also report any open issue that blocks CRW-392 and is missing from all rows.\n${JSON.stringify(ok)}`, { label: 'check', phase: 'Check', agentType: 'ocx-gpt-6-1-sol', effort: 'high', schema: CHECK })
return { projects: ok, check }