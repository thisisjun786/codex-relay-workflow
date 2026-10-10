package spawn

const LeafGuardMarker = "[CRW-LEAF-GUARD]"
const ScopeGuardMarker = "[CRW-SUBAGENT-SCOPE]"
const SkillAffordanceMarker = "[CRW-SKILL-AFFORDANCE]"

// LeafGuardBlock is the oracle guidance with CRW names; this library activates no guard.
const LeafGuardBlock = `[CRW-LEAF-GUARD] You are a LEAF agent with a single bounded task. HARD
CONSTRAINTS from your dispatcher: (1) Do NOT spawn
sub-agents (no spawn_agent calls, no delegation chains). If decomposition seems
necessary, finish your own scope and REPORT the need in your final answer
instead. (2) Do NOT run crw pabcd orchestrate, crw pabcd loop, or goal commands - the
parent session owns all FSM/goal state. (3) Stay inside the task's stated
file/write scope. These dispatcher constraints are enforced by a spawn hook (a
recursive spawn without a grant is DENIED at the tool boundary, regardless of
any delegation guidance you may see). A dispatcher can authorize recursion for
a specific spawn by
including the recursion grant token in the spawn message.
(4) You are NOT in a copy or fork of the workspace: you share the parent's
working directory, branch and HEAD, so your edits are the parent's uncommitted
changes. Stay inside your write scope and do NOT run branch-level git commands
(checkout, switch, branch, stash, reset, rebase, merge, pull) - another agent
may be working in the same tree right now.`

// LeafGuardBlockCoordinator is the oracle guidance with CRW names; this library activates no guard.
const LeafGuardBlockCoordinator = `[CRW-LEAF-GUARD] You are a COORDINATOR agent with a single bounded task. HARD
CONSTRAINTS from your dispatcher:
(1) Recursion is authorized for this task. (2) Do NOT run crw pabcd orchestrate, crw pabcd loop, or goal commands - the
parent session owns all FSM/goal state. (3) Stay inside the task's stated
file/write scope. All remaining constraints still apply.
(4) You share the parent's working directory, branch and HEAD - this is not a
copy. Your edits are the parent's uncommitted changes, and so are your own
children's. Give every child a non-overlapping write scope and do NOT run
branch-level git commands (checkout, switch, branch, stash, reset, rebase,
merge, pull) or let a child run them.`

// V1ScopeBlock is the oracle guidance with CRW names; this library activates no guard.
const V1ScopeBlock = `[CRW-SUBAGENT-SCOPE] This is one bounded delegated task. The parent
owns crw orchestration, loop, and goal state; do not invoke those
commands. Stay within the stated file/write scope and report any
required expansion.
You run in the parent's own working directory, on its branch and HEAD - not a
copy - so your edits are the parent's uncommitted changes. Do not run
branch-level git commands (checkout, switch, branch, stash, reset, rebase,
merge, pull); another agent may be working in the same tree.`

// V1ScopeBlockCoordinator is the oracle guidance with CRW names; this library activates no guard.
const V1ScopeBlockCoordinator = `[CRW-SUBAGENT-SCOPE] This is one bounded delegated task with authorized
recursion. The parent owns crw orchestration, loop, and goal state;
do not invoke those commands. Stay within the stated file/write scope.
You and any child you spawn run in the parent's own working directory, on its
branch and HEAD - not a copy. Keep every write scope non-overlapping and do not
run branch-level git commands (checkout, switch, branch, stash, reset, rebase,
merge, pull).`

// SkillAffordanceBlock is the self-load guidance; a later caller chooses when
// to use it. Catalog omission never removes the guidance itself.
func SkillAffordanceBlock(skillsDir string) string {
	block := SkillAffordanceMarker + " Skill mentions in this task (tokens like\n" +
		"$crw-<name> or $crw:crw-<name>, or [$crw-<name>](skill://...) links)\n" +
		"are NOT auto-loaded on this surface. Before working, read each mentioned\n" +
		"skill yourself: open " + skillsDir + "/<name>/SKILL.md with your file tools and\n" +
		"follow it. If a mentioned skill file does not exist there, note that in\n" +
		"your answer and continue."
	if catalog := BuildLeafSkillCatalog(skillsDir); catalog != "" {
		block += "\n\n" + catalog
	}
	return block
}
