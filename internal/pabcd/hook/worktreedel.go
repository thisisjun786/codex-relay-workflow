package hook

// The worktree deletion guard's shared pieces. The verdict is judged over the command reader's program records in
// worktreedel_judge.go; this file keeps the verdict type, the deny text, the protected-target test and the PreToolUse leg.

import (
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// GuardVerdict is the oracle's allow | deny with a reason; the zero value is allow.
type GuardVerdict struct {
	Deny   bool
	Reason string
}

// basename is what follows the last slash once every backslash is a slash.
func basename(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return p[strings.LastIndexByte(p, '/')+1:]
}

// resolveFrom is path.resolve(base, target) for two segments: an empty target is the base, an absolute one stands alone, a
// relative base is made absolute from the process's working directory, and the result is cleaned.
func resolveFrom(base, target string) string {
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	switch {
	case target == "":
		return abs(base)
	case filepath.IsAbs(target):
		return filepath.Clean(target)
	}
	return filepath.Join(abs(base), target)
}

// isProtectedTarget says whether deleting target (taken from segCwd) deletes the slot, the checkout, the directory the
// segment runs in, or an ancestor of that directory. The oracle compares against the segment's own directory, so a cd to
// an unrelated place moves what is protected along with it (known defect). Its ancestor test is cwd.startsWith(resolved
// + sep), which never holds for the root; the extended walk fixes that.
func isProtectedTarget(target, segCwd string, id WorktreeIdentity, extended bool) bool {
	resolved := canonicalize(resolveFrom(segCwd, target))
	cwd := canonicalize(segCwd)
	if resolved == id.SlotRoot && id.SlotRoot != "" || resolved == id.CheckoutRoot && id.CheckoutRoot != "" || resolved == cwd {
		return true
	}
	prefix := resolved + "/"
	if extended {
		prefix = strings.TrimSuffix(resolved, "/") + "/"
	}
	return strings.HasPrefix(cwd, prefix)
}
func denyReason(what string, id WorktreeIdentity) string {
	slotRoot := id.SlotRoot
	if slotRoot == "" {
		slotRoot = "unknown"
	}
	return strings.Join([]string{
		"[crw: WORKTREE-GUARD-03] blocked `" + what + "`: it deletes this session's",
		"own Codex-app-managed worktree (slot: " + slotRoot + "). This thread",
		"is bound to that worktree; deletion destroys uncommitted work.",
		"Remedies: finish and commit here; rename in place (git switch -c / branch -m);",
		"teardown of THIS session's worktree is done by the user (archive the thread in",
		"the app — snapshot preserved — or remove it from OUTSIDE this session).",
		"See $crw:crw-worktree-guardian.",
	}, " ")
}

// HandleWorktreeGuardPreTool is handleWorktreeGuardPreTool, the PreToolUse leg: the deny line for a Bash command that
// deletes the session's own worktree, else empty (it never fails the tool). A tool name other than Bash, a payload
// without a cwd or a command, and anything that is not one JSON object all pass. The guard sits above the subagent exit,
// so a subagent's turn is checked too.
func HandleWorktreeGuardPreTool(raw string, env host.LookupEnv) string {
	payload := parseRaw(raw)
	if payload["hook_event_name"] != "PreToolUse" {
		return ""
	}
	if tool, _ := payload["tool_name"].(string); tool != "" && tool != "Bash" {
		return ""
	}
	cwd, _ := payload["cwd"].(string)
	input, _ := payload["tool_input"].(map[string]any)
	command, _ := input["command"].(string)
	if cwd == "" || command == "" {
		return ""
	}
	verdict := evaluateCommand(command, shellirPayloadCwd(cwd), detectManagedWorktree(cwd, env))
	if !verdict.Deny {
		return ""
	}
	return editAnswer("deny", verdict.Reason, verdict.Reason)
}
