package settings

// RelayChildReaches is whether a relay child created under sandbox and policy can connect to the relay daemon's
// control.sock, where it reports (CRW-1296). Codex refuses that connect (EPERM, codex-cli 0.154.0) in a readOnly
// sandbox and in a workspace-write one without network access, so a child reaches the socket only under
// danger-full-access or a workspaceWrite policy granting networkAccess. sandbox is the mode a creation names
// ("read-only", "workspace-write", "danger-full-access"; "" when only a policy is known), policy the sandbox policy
// object ({"type": "workspaceWrite", "networkAccess": true}), nil when none is named. An explicit readOnly policy
// does not reach even with networkAccess: a relay child writes its work.
//
// It is the one rule: bridge creation (create_thread, create_worktree_thread) and relay register --child-settings both ask it.
func RelayChildReaches(sandbox string, policy map[string]any) bool {
	if sandbox == "read-only" {
		return false
	}
	return (sandbox == "danger-full-access" && policy == nil) || policy["type"] == "dangerFullAccess" || (policy["type"] == "workspaceWrite" && policy["networkAccess"] == true)
}

// RelayChildSandboxes names the two sandboxes in which a relay child reaches the relay daemon.
const RelayChildSandboxes = "danger-full-access, or workspace-write with network access (a workspaceWrite policy granting networkAccess)"
