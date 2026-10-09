package spawn

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

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

// BuildLeafSkillCatalog returns sorted leaf-safe metadata. Like the oracle it
// scans the first 1024 UTF-16 units, without requiring YAML frontmatter fences.
// Read failures omit the skill; its displayed name need not equal its folder.
func BuildLeafSkillCatalog(skillsDir string) string {
	root, err := os.OpenRoot(skillsDir)
	if err != nil {
		return ""
	}
	defer root.Close()
	folders, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return ""
	}
	boundary := `(?:^|[\n\r\x{2028}\x{2029}])`
	end := `(?:$|[\n\r\x{2028}\x{2029}])`
	namePattern := regexp.MustCompile(boundary + `name:` + spawnInlineJSSpace + `*([^\n\r\x{2028}\x{2029}]+)` + end)
	descPattern := regexp.MustCompile(boundary + `description:` + spawnInlineJSSpace + `*"?([^"\n]+)"?` + end)
	var entries []string
	for _, folder := range folders {
		if !folder.IsDir() || !slices.Contains(LeafSafeSkillFolders(), folder.Name()) {
			continue
		}
		body, ok := spawnInlineReadSkill(root, skillsDir, folder.Name())
		if !ok {
			continue
		}
		head := spawnInlineUTF16Prefix(body, 1024)
		name := namePattern.FindStringSubmatch(head)
		if name == nil {
			continue
		}
		desc := ""
		if m := descPattern.FindStringSubmatch(head); m != nil {
			desc = spawnInlineUTF16Prefix(text.Trim(m[1]), 120)
		}
		entries = append(entries, "- "+text.Trim(name[1])+": "+desc)
	}
	if len(entries) == 0 {
		return ""
	}
	return "Available skills (self-load from " + skillsDir + "/<name>/SKILL.md):\n" + strings.Join(entries, "\n")
}

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

// EvidenceAssignmentMarker opens the block that tells a child where its evidence goes (CRW-1115). The spawn hook writes it with the
// id of the assignment it recorded for the parent's session: [CRW-EVIDENCE-ASSIGNMENT:<id>].
const EvidenceAssignmentMarker = "[CRW-EVIDENCE-ASSIGNMENT"

// The packet lines a parent writes to assign a child's evidence location: the absolute path of the worker's assigned worktree,
// and "none" when the packet allows the child no evidence write at all (a one-file or read-only scope).
const (
	WorktreePacketField = "CRW-WORKTREE:"
	EvidencePacketField = "CRW-EVIDENCE:"
)

// EvidenceAssignmentBlock is the block injected after the guard for a recorded assignment. A tree assignment names the approved
// receipt directory of the assigned tree; a no-write assignment names the scope-conflict line instead, so the child never widens
// its scope to satisfy the gate.
func EvidenceAssignmentBlock(id, root string, none bool) string {
	block := EvidenceAssignmentMarker + ":" + id + "]"
	if root != "" {
		block += " Your assigned worktree is " + root + ". Pass it as the shell workdir on every\n" +
			"command; your native cwd is still the parent's tree."
	}
	if none {
		return block + " Your packet allows you no evidence write: do\n" +
			"not create a receipt or widen your scope to make one. Report exactly what you\n" +
			"checked and what you could not, and make the LAST line of your reply exactly\n" +
			"`EVIDENCE_SCOPE_CONFLICT: " + id + "`. Your parent then verifies the work itself."
	}
	return block + " Record your evidence receipt (the checks you ran, their output and your\n" +
		"judgement) under " + filepath.Join(root, ".crw", "evidence") + "/ - that directory is in\n" +
		"your write scope for the receipt only - and make the LAST line of your reply\n" +
		"exactly `EVIDENCE_RECORDED: <absolute path of that file>`."
}

// spawnEvidenceRequest reads the assignment lines of the caller's packet. A line counts when, after white space, it starts with the
// field name. present is false when neither line is there. More than one worktree, or an evidence value other than none, is an
// error: the parent's packet is ambiguous and the spawn is refused rather than guessed.
func spawnEvidenceRequest(message string) (worktree string, none, present bool, err error) {
	for _, line := range strings.FieldsFunc(message, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = text.Trim(line)
		if value, ok := strings.CutPrefix(line, WorktreePacketField); ok {
			value = text.Trim(value)
			if worktree != "" && worktree != value || value == "" {
				return "", false, false, fmt.Errorf("the packet must name one assigned worktree on its %s line", WorktreePacketField)
			}
			worktree, present = value, true
		} else if value, ok := strings.CutPrefix(line, EvidencePacketField); ok {
			if strings.ToLower(text.Trim(value)) != "none" {
				return "", false, false, fmt.Errorf("%s takes only the value none", EvidencePacketField)
			}
			none, present = true, true
		}
	}
	return worktree, none, present, nil
}
